//go:build coreslot_testhooks && unix

package conformance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// The sampler checks the invariants I1, I2, I3 and I7 of a state directory, as the hold bodies
// carry them, while clients of every kind run, die and stop in it. Each sample is taken inside
// the admission section, so no admission is in flight while it reads. A violation is reported
// only when the same violation is found by two samples at least sampleGap apart: releases happen
// outside the section, and so do probes (a probe's momentary lock makes a dead hold look live).
// The sampler owns the configuration: Resize rewrites it inside the section, so that every
// admission is linearized before or after the size change and the I3 bound can follow it.
// Because Resize writes the configuration inside the section, a client that read the
// configuration before entering the section and admits on that stale size after the rewrite is
// not caught as such: the sampler sees it only if the stale size takes the reservations above
// the bound.

// sampleGap is the least time between two samples whose findings are compared.
const sampleGap = 100 * time.Millisecond

// Violation is one invariant violation found by two consecutive samples.
type Violation struct {
	Invariant string // I1, I2, I3, I7, or "fail-open" for an unbudgeted hold
	Key       string // the identity compared between samples
	Msg       string
	At        time.Time
}

func (v Violation) String() string {
	return fmt.Sprintf("%s at %s: %s", v.Invariant, v.At.Format("15:04:05.000"), v.Msg)
}

// liveHold is a hold whose lock is held, with its decoded body and its file's inode.
type liveHold struct {
	Name  string
	Body  wire.Hold
	inode [2]uint64
}

// observation is what one sample read inside the section.
type observation struct {
	at        time.Time
	holds     []liveHold
	malformed []string // live holds whose body does not decode
}

// Sampler samples one state directory. Its methods are safe for concurrent use. Every amount is
// in MiB.
type Sampler struct {
	// Request returns the need behind a hold's label, for I7. Holds whose label it does not know
	// are not checked for I7.
	Request func(label string) (needMiB int64, ok bool)

	mu          sync.Mutex
	dir         string
	scratch     string // where a configuration is written before it is renamed into place
	st          *store.Store
	size        int64
	ceiling     int64                // I3 bound: the size, or the reservations a shrink left above it
	inodes      map[string][2]uint64 // each live hold's inode when the sampler first saw it
	prev        map[string]Violation // candidates of the previous sample
	reported    map[string]bool      // violations reported and still found by the latest sample
	violations  []Violation
	last        time.Time // when the previous sample was taken
	samples     int
	missed      int
	configMtime time.Time
}

// NewSampler writes the configuration size into e's state directory, makes sure FORMAT is written
// and returns a sampler of the directory.
func NewSampler(t *testing.T, e Env, sizeMiB int64) *Sampler {
	t.Helper()
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Sampler{dir: e.Dir, scratch: t.TempDir(),
		inodes: map[string][2]uint64{}, prev: map[string]Violation{}, reported: map[string]bool{}}
	var err error
	if s.st, err = store.Open(e.Dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), startWait)
	defer cancel()
	sec, err := s.st.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if _, ok, err := sec.Format(); err != nil || !ok {
		t.Fatalf("FORMAT: understood %v, %v", ok, err)
	}
	if err := s.writeConfig(sizeMiB); err != nil {
		t.Fatal(err)
	}
	s.size, s.ceiling = sizeMiB, sizeMiB
	return s
}

// writeConfig replaces config.toml atomically, with an mtime strictly after the previous one,
// so that a client caching the configuration by mtime and size sees every change.
func (s *Sampler) writeConfig(sizeMiB int64) error {
	tmp := filepath.Join(s.scratch, "config.toml")
	if err := os.WriteFile(tmp, []byte(capacityConfig(sizeMiB)), 0o600); err != nil {
		return err
	}
	m := time.Now()
	if !m.After(s.configMtime) {
		m = s.configMtime.Add(time.Millisecond)
	}
	s.configMtime = m
	if err := os.Chtimes(tmp, m, m); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, store.ConfigFile))
}

// Size returns the size the configuration declares now.
func (s *Sampler) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// Resize rewrites the configuration inside the admission section. Every admission is then
// either before the change, with the old size, or after it, with the new one. A shrink may
// leave more reserved than the new size; the I3 bound becomes that amount and falls with it,
// until it reaches the size. It returns what the live holds reserve at the change.
func (s *Sampler) Resize(ctx context.Context, sizeMiB int64) (reserved int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, err := s.st.Enter(ctx)
	if err != nil {
		return 0, err
	}
	defer sec.Leave()
	if err := s.writeConfig(sizeMiB); err != nil {
		return 0, err
	}
	obs, err := s.observe()
	if err != nil {
		return 0, err
	}
	reserved = s.reserved(obs)
	// What a shrink leaves above the new size is bounded by what the old size allowed: a resize
	// never makes an I3 violation (or a live admission) part of the bound.
	s.ceiling = max(sizeMiB, min(reserved, max(s.size, s.ceiling)))
	s.size = sizeMiB
	return reserved, nil
}

// reserved is what obs's live holds reserve; a lone hold is at most the most any admission could
// have granted it, since a need above the capacity runs alone.
func (s *Sampler) reserved(obs observation) int64 {
	var n int64
	for _, h := range obs.holds {
		n += h.Body.NeedMiB
	}
	if len(obs.holds) == 1 {
		n = min(n, max(s.size, s.ceiling))
	}
	return n
}

// Sample takes one sample inside the admission section, at least sampleGap after the previous
// one, and returns the violations it confirms: those the previous sample found too.
func (s *Sampler) Sample(ctx context.Context) ([]Violation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wait := time.Until(s.last.Add(sampleGap)); wait > 0 {
		time.Sleep(wait)
	}
	sec, err := s.st.Enter(ctx)
	if err != nil {
		s.missed++
		return nil, err
	}
	obs, err := s.observe()
	sec.Leave()
	if err != nil {
		s.missed++
		return nil, err
	}
	s.last = obs.at
	s.samples++
	found := s.check(obs)
	var confirmed []Violation
	for key, v := range found {
		if _, again := s.prev[key]; again && !s.reported[key] {
			s.reported[key] = true
			confirmed = append(confirmed, v)
		}
	}
	// A violation is reported once while it lasts; once a sample no longer finds it, a later
	// occurrence is a new one.
	for key := range s.reported {
		if _, still := found[key]; !still {
			delete(s.reported, key)
		}
	}
	slices.SortFunc(confirmed, func(a, b Violation) int { return strings.Compare(a.Key, b.Key) })
	s.prev = found
	s.violations = append(s.violations, confirmed...)
	return confirmed, nil
}

// Run samples until ctx ends.
func (s *Sampler) Run(ctx context.Context) {
	for ctx.Err() == nil {
		_, _ = s.Sample(ctx)
	}
}

// Violations returns every violation confirmed so far, and how many samples were taken and
// missed (the section could not be entered before ctx ended, or reading failed).
func (s *Sampler) Violations() (v []Violation, samples, missed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.violations), s.samples, s.missed
}

// observe reads the live holds. The caller is inside the section.
func (s *Sampler) observe() (observation, error) {
	obs := observation{at: time.Now()}
	entries, _, err := s.st.List(store.HoldsDir)
	if err != nil {
		return obs, err
	}
	for _, en := range entries {
		dead, err := s.st.Probe(store.HoldsDir, en.Name)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && dead) {
			continue
		}
		if err != nil {
			return obs, err
		}
		path := filepath.Join(s.dir, store.HoldsDir, en.Name)
		data, err := os.ReadFile(path)
		fi, serr := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(serr, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return obs, err
		}
		if serr != nil {
			return obs, serr
		}
		h, err := wire.DecodeHold(data)
		if err != nil {
			obs.malformed = append(obs.malformed, en.Name)
			continue
		}
		st := fi.Sys().(*syscall.Stat_t)
		obs.holds = append(obs.holds, liveHold{Name: en.Name, Body: h, inode: [2]uint64{uint64(st.Dev), uint64(st.Ino)}})
	}
	return obs, nil
}

// check returns the violations obs shows, by key, and moves the I3 bound. The caller holds mu.
func (s *Sampler) check(obs observation) map[string]Violation {
	found := map[string]Violation{}
	add := func(inv, key, format string, args ...any) {
		found[key] = Violation{Invariant: inv, Key: key, Msg: fmt.Sprintf(format, args...), At: obs.at}
	}

	// I1: a live hold keeps its inode: its body is rewritten in place, never replaced. The inode
	// a hold has when the sampler first sees it live is its identity.
	live := map[string]bool{}
	for _, h := range obs.holds {
		live[h.Name] = true
		if first, seen := s.inodes[h.Name]; !seen {
			s.inodes[h.Name] = h.inode
		} else if first != h.inode {
			add("I1", "I1 inode "+h.Name, "live hold %s changed inode from %v to %v", h.Name, first, h.inode)
		}
	}
	for name := range s.inodes {
		if !live[name] {
			delete(s.inodes, name)
		}
	}

	// I3: what the live holds reserve stays within the bound.
	n := s.reserved(obs)
	if n > max(s.size, s.ceiling) {
		add("I3", "I3 mem", "%d MiB reserved; size %d, bound %d", n, s.size, s.ceiling)
	}
	s.ceiling = max(s.size, min(s.ceiling, n))

	// I7: an ordinary admission reserves the need asked.
	for _, h := range obs.holds {
		if h.Body.NeedMiB == 0 {
			add("fail-open", "fail-open "+h.Name, "hold %s is unbudgeted (a client failed open)", h.Name)
			continue
		}
		if s.Request == nil {
			continue
		}
		if need, ok := s.Request(h.Body.Label); ok && h.Body.NeedMiB != need {
			add("I7", "I7 "+h.Name, "hold %s reserves %d MiB for a need of %d", h.Name, h.Body.NeedMiB, need)
		}
	}
	// I2: a live hold's body says what it reserves.
	for _, name := range obs.malformed {
		add("I2", "malformed "+name, "live hold %s has a malformed body", name)
	}
	return found
}

// Leftovers returns, from one observation inside the section, the live holds: after every
// client is gone there is none.
func (s *Sampler) Leftovers(ctx context.Context) (holds []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, err := s.st.Enter(ctx)
	if err != nil {
		return nil, err
	}
	obs, err := s.observe()
	sec.Leave()
	if err != nil {
		return nil, err
	}
	for _, h := range obs.holds {
		holds = append(holds, h.Name)
	}
	return append(holds, obs.malformed...), nil
}

// ---------------------------------------------------------------- the sampler's own test

// fakeHold is the body of a constructed hold labeled label that reserves units of 128 MiB.
func fakeHold(label string, units int64) wire.Hold {
	return wire.Hold{Label: label, NeedMiB: units * 128}
}

// fakeHolds publishes the holds inside the section, as they are given: that is how a defective
// client is built. close releases them.
func fakeHolds(t *testing.T, e Env, holds ...wire.Hold) (close func()) {
	t.Helper()
	var files []*os.File
	inSection(t, e, func(sec *store.Section) {
		for _, h := range holds {
			h.PID, h.StartNS = os.Getpid(), store.NowNS()
			f, _, err := sec.PublishHold(h, store.NextSeq())
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
	})
	return func() {
		for _, f := range files {
			f.Close()
		}
	}
}

// fakeRawHold publishes, inside the section, a live hold labeled label whose body is body as
// given, valid or not. close releases it.
func fakeRawHold(t *testing.T, e Env, label string, body []byte) (close func()) {
	t.Helper()
	var f *os.File
	inSection(t, e, func(sec *store.Section) {
		var err error
		name := wire.HoldName(store.NowNS(), os.Getpid(), store.NextSeq(), label)
		if f, err = sec.Publish(store.HoldsDir, name, body, true); err != nil {
			t.Fatal(err)
		}
	})
	return func() { f.Close() }
}

// inSection runs build inside the admission section and bumps gen after it.
func inSection(t *testing.T, e Env, build func(*store.Section)) {
	t.Helper()
	st, err := store.Open(e.Dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), startWait)
	defer cancel()
	sec, err := st.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	build(sec)
	if err := sec.BumpGen(); err != nil {
		t.Fatal(err)
	}
}

// TestSamplerCatchesAViolation passes the sampler a valid history of real runs, then builds
// real violations of I1, I2, I3 and I7 and a fail-open hold in the state directory and checks
// that the sampler reports each one, and only it, once two samples saw it; what a shrink left
// above the size is not reported.
func TestSamplerCatchesAViolation(t *testing.T) {
	e := State(t)
	s := NewSampler(t, e, 4*128)
	s.Request = stormRequest
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sample := func() []string {
		t.Helper()
		vs, err := s.Sample(ctx)
		if err != nil {
			t.Fatalf("sample: %v", err)
		}
		var keys []string
		for _, v := range vs {
			keys = append(keys, v.Key)
			t.Logf("reported: %v", v)
		}
		return keys
	}
	// expect takes two samples: the first reports nothing new, the second exactly want.
	expect := func(what string, want ...string) {
		t.Helper()
		slices.Sort(want)
		if got := sample(); len(got) != 0 {
			t.Errorf("%s: the first sample already reported %v", what, got)
		}
		if got := sample(); !slices.Equal(got, want) {
			t.Errorf("%s: two samples reported %v, want %v", what, got, want)
		}
	}
	// settle takes one sample after a constructed violation is removed, so that the next case
	// starts from a sample without it.
	settle := func() {
		t.Helper()
		if got := sample(); len(got) != 0 {
			t.Errorf("after the violation was removed, the sampler reported %v", got)
		}
	}
	resize := func(units int64) {
		t.Helper()
		if _, err := s.Resize(ctx, units*128); err != nil {
			t.Fatal(err)
		}
	}

	// A valid history: two runs hold, a third waits for more than is free, then each is
	// released in turn, with samples throughout.
	a := e.Run("v1.cli.m2", "--mem", "256M")
	e.WaitHold("v1.cli.m2", startWait)
	b := e.Run("v2.cli.m2", "--mem", "256M")
	e.WaitHold("v2.cli.m2", startWait)
	c := e.Run("v3.cli.m3", "--mem", "384M")
	e.WaitTicket("v3.cli.m3")
	expect("two holds and a waiter")
	e.ReleaseChild("v1.cli.m2")
	a.ExitCode(0)
	e.ReleaseChild("v2.cli.m2")
	b.ExitCode(0)
	e.WaitHold("v3.cli.m3", startWait)
	expect("the waiter admitted")
	e.ReleaseChild("v3.cli.m3")
	c.ExitCode(0)
	expect("everything released")

	t.Run("more reserved than the size is reported (I3)", func(t *testing.T) {
		release := fakeHolds(t, e, fakeHold("x.cli.m3", 3), fakeHold("y.cli.m3", 3))
		expect("6 units held under size 4", "I3 mem")
		release()
		settle()
	})
	t.Run("a shrink leaves reservations above the size; the bound falls with them (I3)", func(t *testing.T) {
		releaseA := fakeHolds(t, e, fakeHold("ra.cli.m3", 3))
		releaseB := fakeHolds(t, e, fakeHold("rb.cli.m1", 1))
		resize(2)
		expect("4 units held after a shrink to 2")
		releaseB()
		settle() // the bound falls to 3
		releaseC := fakeHolds(t, e, fakeHold("rc.cli.m1", 1))
		expect("a unit taken while 3 are reserved above size 2", "I3 mem")
		releaseA()
		releaseC()
		settle()
		resize(4)
	})
	t.Run("a hold that reserves other than its need is reported (I7)", func(t *testing.T) {
		release := fakeHolds(t, e, fakeHold("w.cli.m3", 1))
		expect("1 unit reserved for mem=3", "I7 "+holdNamed(t, e, "w.cli.m3"))
		release()
		settle()
	})
	t.Run("a need above the size reserves it whole, alone", func(t *testing.T) {
		release := fakeHolds(t, e, fakeHold("k.cli.m9", 9))
		expect("a clamped mem=9 alone")
		release()
		settle()
	})
	t.Run("an unbudgeted hold is reported (fail-open)", func(t *testing.T) {
		release := fakeHolds(t, e, fakeHold("f.cli.m1", 0))
		expect("a fail-open hold", "fail-open "+holdNamed(t, e, "f.cli.m1"))
		release()
		settle()
	})
	t.Run("a live hold with a malformed body is reported (I2)", func(t *testing.T) {
		release := fakeRawHold(t, e, "m", []byte(`{"label":"m"}`))
		entries, _ := os.ReadDir(filepath.Join(e.Dir, store.HoldsDir))
		name := ""
		for _, en := range entries {
			if strings.HasSuffix(en.Name(), "-m") {
				name = en.Name()
			}
		}
		expect("a malformed hold", "malformed "+name)
		release()
		settle()
	})
	t.Run("a live hold's file replaced is reported (I1)", func(t *testing.T) {
		release := fakeHolds(t, e, fakeHold("i.cli.m1", 1))
		name := holdNamed(t, e, "i.cli.m1")
		settle() // the sampler has seen the hold's inode
		path := filepath.Join(e.Dir, store.HoldsDir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".new", body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".new", path); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path) // the replacement is held too, as a client that rewrote it would
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		expect("the hold replaced by a new file", "I1 inode "+name)
		release()
		f.Close()
		settle()
	})
}

// holdNamed returns the name of the hold labeled label.
func holdNamed(t *testing.T, e Env, label string) string {
	t.Helper()
	h, ok := e.Hold(label)
	if !ok {
		t.Fatalf("no hold labelled %s", label)
	}
	return h.Name
}
