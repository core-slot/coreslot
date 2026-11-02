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

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// The sampler checks the invariants I1, I2 and I3 of a state directory, as the hold bodies
// carry them, while clients of every kind run, die and stop in it. Each sample is taken inside
// the admission section, so no admission is in flight while it reads. A violation is reported
// only when the same violation is found by two samples at least sampleGap apart: releases happen
// outside the section, and so do probes (a probe's momentary lock makes a dead hold look live).
// The sampler owns the configuration: SetRoom rewrites it inside the section, so that every
// admission reads either the room before the change or the room after it.
//
// I3 is checked at the admissions two samples bracket: a hold the later sample lists first was
// published between them, and every hold live in both was live at that admission, so the new
// hold's need must fit what those left of the room. The holds published or released between the
// samples are left out, and each kept hold counts the larger footprint of its two bodies (a body
// is rewritten once per period): both can only leave more room than the admission saw. When no
// hold is live in both, the idle rule may have admitted it, and nothing is checked.

// sampleGap is the least time between two samples whose findings are compared.
const sampleGap = 100 * time.Millisecond

// Violation is one invariant violation found by two consecutive samples.
type Violation struct {
	Invariant string // I1, I2, I3, or "fail-open" for an unbudgeted hold
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
	listed    map[string]bool // every hold file listed, live or not
	holds     []liveHold
	malformed []string // live holds whose body does not decode
}

// admission is an I3 finding: a hold admitted past the room its witnesses left. The next sample
// finds it again while every witness is still live, so that none looked live for a probe only.
type admission struct {
	witnesses []string
	msg       string
}

// Sampler samples one state directory. Its methods are safe for concurrent use. Every amount is
// in MiB.
type Sampler struct {
	mu          sync.Mutex
	dir         string
	scratch     string // where a configuration is written before it is renamed into place
	st          *store.Store
	room        int64                // what the configuration leaves while no hold is live
	prevRoom    int64                // the room at the previous sample
	before      observation          // the previous sample
	admissions  map[string]admission // the previous sample's I3 findings, by key
	inodes      map[string][2]uint64 // each live hold's inode when the sampler first saw it
	prev        map[string]Violation // candidates of the previous sample
	reported    map[string]bool      // violations reported and still found by the latest sample
	violations  []Violation
	samples     int
	missed      int
	configMtime time.Time
}

// NewSampler writes a configuration that leaves roomMiB into e's state directory, makes sure
// FORMAT is written and returns a sampler of the directory.
func NewSampler(t *testing.T, e Env, roomMiB int64) *Sampler {
	t.Helper()
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Sampler{dir: e.Dir, scratch: t.TempDir(), admissions: map[string]admission{},
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
	if err := s.writeConfig(roomMiB); err != nil {
		t.Fatal(err)
	}
	s.room, s.prevRoom = roomMiB, roomMiB
	return s
}

// writeConfig replaces config.toml atomically, with an mtime strictly after the previous one,
// so that a client caching the configuration by mtime and size sees every change.
func (s *Sampler) writeConfig(roomMiB int64) error {
	tmp := filepath.Join(s.scratch, "config.toml")
	if err := os.WriteFile(tmp, []byte(roomConfig(roomMiB)), 0o600); err != nil {
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

// Room returns what the configuration leaves now while no hold is live.
func (s *Sampler) Room() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.room
}

// SetRoom rewrites the configuration inside the admission section, so that it leaves roomMiB.
// It returns what the live holds reserve at the change.
func (s *Sampler) SetRoom(ctx context.Context, roomMiB int64) (reserved int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, err := s.st.Enter(ctx)
	if err != nil {
		return 0, err
	}
	defer sec.Leave()
	if err := s.writeConfig(roomMiB); err != nil {
		return 0, err
	}
	obs, err := s.observe()
	if err != nil {
		return 0, err
	}
	for _, h := range obs.holds {
		reserved += h.Body.NeedMiB
	}
	s.room = roomMiB
	return reserved, nil
}

// Sample takes one sample inside the admission section, at least sampleGap after the previous
// one, and returns the violations it confirms: those the previous sample found too.
func (s *Sampler) Sample(ctx context.Context) ([]Violation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wait := time.Until(s.before.at.Add(sampleGap)); wait > 0 {
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
	s.samples++
	found := s.check(obs)
	s.before, s.prevRoom = obs, s.room
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

// observe lists the holds and reads the live ones. The caller is inside the section.
func (s *Sampler) observe() (observation, error) {
	obs := observation{at: time.Now(), listed: map[string]bool{}}
	entries, _, err := s.st.List(store.HoldsDir)
	if err != nil {
		return obs, err
	}
	for _, en := range entries {
		obs.listed[en.Name] = true
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

// check returns the violations obs shows, by key. The caller holds mu.
func (s *Sampler) check(obs observation) map[string]Violation {
	found := map[string]Violation{}
	add := func(inv, key, format string, args ...any) {
		found[key] = Violation{Invariant: inv, Key: key, Msg: fmt.Sprintf(format, args...), At: obs.at}
	}
	live := map[string]liveHold{}
	for _, h := range obs.holds {
		live[h.Name] = h
	}

	// I1: a live hold keeps its inode: its body is rewritten in place, never replaced. The inode
	// a hold has when the sampler first sees it live is its identity.
	for _, h := range obs.holds {
		if first, seen := s.inodes[h.Name]; !seen {
			s.inodes[h.Name] = h.inode
		} else if first != h.inode {
			add("I1", "I1 inode "+h.Name, "live hold %s changed inode from %v to %v", h.Name, first, h.inode)
		}
	}
	for name := range s.inodes {
		if _, ok := live[name]; !ok {
			delete(s.inodes, name)
		}
	}

	// I3: the previous sample's findings stand while their witnesses live. Samples a period or
	// more apart may have missed a footprint: the admissions between them are not checked.
	for key, a := range s.admissions {
		if !slices.ContainsFunc(a.witnesses, func(n string) bool { _, ok := live[n]; return !ok }) {
			add("I3", key, "%s", a.msg)
		}
	}
	s.admissions = map[string]admission{}
	if obs.at.Sub(s.before.at) < time.Duration(memsample.PeriodNS) {
		room, witnesses := max(s.room, s.prevRoom), []string(nil)
		for _, b := range s.before.holds {
			if h, ok := live[b.Name]; ok {
				room -= max(0, h.Body.NeedMiB-max(h.Body.FootprintMiB, b.Body.FootprintMiB))
				witnesses = append(witnesses, b.Name)
			}
		}
		for _, h := range obs.holds {
			if witnesses != nil && !s.before.listed[h.Name] && h.Body.NeedMiB > room {
				a := admission{witnesses, fmt.Sprintf("hold %s admitted for %d MiB with %d MiB of room left by %v",
					h.Name, h.Body.NeedMiB, room, witnesses)}
				s.admissions["I3 "+h.Name] = a
				add("I3", "I3 "+h.Name, "%s", a.msg)
			}
		}
	}

	for _, h := range obs.holds {
		if h.Body.NeedMiB == 0 {
			add("fail-open", "fail-open "+h.Name, "hold %s is unbudgeted (a client failed open)", h.Name)
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

// fakeHold is the body of a constructed hold labeled label that reserves needMiB.
func fakeHold(label string, needMiB int64) wire.Hold {
	return wire.Hold{Label: label, NeedMiB: needMiB}
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
// real violations of I1, I2 and I3 and a fail-open hold in the state directory and checks that
// the sampler reports each one, and only it, once two samples saw it; a soft room, the idle rule
// and a room set below what is reserved are not reported.
func TestSamplerCatchesAViolation(t *testing.T) {
	e := State(t)
	s := NewSampler(t, e, 512)
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
	// settle takes one sample after a constructed violation is removed, or before one is built,
	// so that the next case starts from a sample without it.
	settle := func() {
		t.Helper()
		if got := sample(); len(got) != 0 {
			t.Errorf("the sampler reported %v", got)
		}
	}
	setRoom := func(mib int64) {
		t.Helper()
		if _, err := s.SetRoom(ctx, mib); err != nil {
			t.Fatal(err)
		}
	}

	// A valid history: two runs hold, a third waits for more than is free, then each is
	// released in turn, with samples throughout.
	a := e.Run("v1", "--mem", "256M")
	e.WaitHold("v1", startWait)
	b := e.Run("v2", "--mem", "256M")
	e.WaitHold("v2", startWait)
	c := e.Run("v3", "--mem", "384M")
	e.WaitTicket("v3")
	expect("two holds and a waiter")
	e.ReleaseChild("v1")
	a.ExitCode(0)
	e.ReleaseChild("v2")
	b.ExitCode(0)
	e.WaitHold("v3", startWait)
	expect("the waiter admitted")
	e.ReleaseChild("v3")
	c.ExitCode(0)
	expect("everything released")

	t.Run("an admission past the room is reported (I3)", func(t *testing.T) {
		releaseX := fakeHolds(t, e, fakeHold("x", 384))
		settle()
		releaseY := fakeHolds(t, e, fakeHold("y", 384))
		expect("384 MiB admitted with 128 MiB of room", "I3 "+holdNamed(t, e, "y"))
		releaseX()
		releaseY()
		settle()
	})
	t.Run("a footprint gives its need back to the room (I3)", func(t *testing.T) {
		x := fakeHold("x", 384)
		x.FootprintMiB = 384
		releaseX := fakeHolds(t, e, x)
		settle()
		releaseY := fakeHolds(t, e, fakeHold("y", 384))
		expect("384 MiB admitted beside a hold whose footprint covers its need")
		releaseX()
		releaseY()
		settle()
	})
	t.Run("a lone hold takes what it needs (the idle rule)", func(t *testing.T) {
		release := fakeHolds(t, e, fakeHold("k", 1152))
		expect("1152 MiB alone")
		release()
		settle()
	})
	t.Run("a room set below what is reserved is no violation, an admission then is (I3)", func(t *testing.T) {
		releaseA := fakeHolds(t, e, fakeHold("ra", 384))
		releaseB := fakeHolds(t, e, fakeHold("rb", 128))
		setRoom(256)
		expect("512 MiB held after the room is set to 256")
		releaseC := fakeHolds(t, e, fakeHold("rc", 128))
		expect("128 MiB admitted while 512 are held in a room of 256", "I3 "+holdNamed(t, e, "rc"))
		releaseA()
		releaseB()
		releaseC()
		settle()
		setRoom(512)
	})
	t.Run("an unbudgeted hold is reported (fail-open)", func(t *testing.T) {
		release := fakeHolds(t, e, fakeHold("f", 0))
		expect("a fail-open hold", "fail-open "+holdNamed(t, e, "f"))
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
		release := fakeHolds(t, e, fakeHold("i", 128))
		name := holdNamed(t, e, "i")
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
