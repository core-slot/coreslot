//go:build coreslot_testhooks && unix

package conformance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// The sampler checks the invariants I1, I2, I3 and I7 of a state
// directory while clients of every kind run, die and stop in it. Each sample is taken inside
// the admission section, so no admission is in flight while it reads. A violation of I1, I3 or
// I7 is reported only when the same violation is found by two samples at least sampleGap
// apart: releases happen outside the section, and so do probes (a probe's momentary lock makes
// a free token look locked). I2 is checked as section 10.2 states it: every locked token must
// be listed by a live hold, or be inside a token-only grant the test itself started, or be
// released by the next sample; a token locked and listed by no live hold in two consecutive
// samples is reported. The sampler owns the configuration: Resize rewrites it inside the
// section, so that every admission is linearized before or after the size change and the I3
// bound can follow it.

// sampleGap is the least time between two samples whose findings are compared.
const sampleGap = 100 * time.Millisecond

// Token is one token file: a resource and an index.
type Token struct {
	Res string
	Idx int
}

func (k Token) String() string { return fmt.Sprintf("%s/%d", k.Res, k.Idx) }

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

// liveHold is a hold whose lock is held, with its decoded body.
type liveHold struct {
	Name string
	Body wire.Hold
}

// observation is what one sample read inside the section.
type observation struct {
	at          time.Time
	locked      map[Token]bool
	lockedCount map[string]int
	inodes      map[Token][2]uint64
	holds       []liveHold
	malformed   []string // live holds whose body does not decode
}

// Sampler samples one state directory. Its methods are safe for concurrent use.
type Sampler struct {
	// Exempt reports a locked token, listed by no live hold for unlisted so far, that belongs
	// to a token-only grant the test itself started (it publishes nothing, so no hold lists
	// it), or may still belong to one whose tokens the test cannot tell yet. Nil exempts
	// nothing.
	Exempt func(k Token, inode [2]uint64, unlisted time.Duration) bool
	// Request returns the request behind a hold's label, for I7. Holds whose label it does not
	// know are not checked for I7.
	Request func(label string) (map[string]spec.Range, bool)

	mu          sync.Mutex
	dir         string
	scratch     string // where a configuration is written before it is renamed into place
	st          *store.Store
	sizes       map[string]int
	ceiling     map[string]int // I3 bound: the size, or the locked count a shrink left above it
	inodes      map[Token][2]uint64
	unlisted    map[Token]time.Time  // since when each locked token is listed by no live hold
	prev        map[string]Violation // candidates of the previous sample
	reported    map[string]bool      // violations reported and still found by the latest sample
	violations  []Violation
	last        time.Time // when the previous sample was taken
	samples     int
	missed      int
	configMtime time.Time
}

// NewSampler writes the configuration sizes into e's state directory, makes sure FORMAT names
// format 1 (so that a client implementing another format runs token-only instead of writing
// its own), and returns a sampler of the directory.
func NewSampler(t *testing.T, e Env, sizes map[string]int) *Sampler {
	t.Helper()
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Sampler{dir: e.Dir, scratch: t.TempDir(), inodes: map[Token][2]uint64{},
		unlisted: map[Token]time.Time{}, prev: map[string]Violation{}, reported: map[string]bool{}}
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
	if _, ok, err := sec.Format([]int{1}); err != nil || !ok {
		t.Fatalf("FORMAT: understood %v, %v", ok, err)
	}
	if err := s.writeConfig(sizes); err != nil {
		t.Fatal(err)
	}
	s.sizes, s.ceiling = maps.Clone(sizes), maps.Clone(sizes)
	return s
}

// configBody is the configuration declaring sizes.
func configBody(sizes map[string]int) string {
	var b strings.Builder
	b.WriteString("[resources]\n")
	for _, r := range slices.Sorted(maps.Keys(sizes)) {
		fmt.Fprintf(&b, "%s = %d\n", r, sizes[r])
	}
	return b.String()
}

// writeConfig replaces config.toml atomically, with an mtime strictly after the previous one,
// so that a client caching the configuration by mtime and size sees every change.
func (s *Sampler) writeConfig(sizes map[string]int) error {
	tmp := filepath.Join(s.scratch, "config.toml")
	if err := os.WriteFile(tmp, []byte(configBody(sizes)), 0o600); err != nil {
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

// Sizes returns the sizes the configuration declares now.
func (s *Sampler) Sizes() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.sizes)
}

// Resize rewrites the configuration inside the admission section. Every admission is then
// either before the change, with the old sizes, or after it, with the new ones. A shrink may
// leave more tokens locked than the new size; the I3 bound becomes that locked count and falls
// with it, until it reaches the size.
func (s *Sampler) Resize(ctx context.Context, sizes map[string]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, err := s.st.Enter(ctx)
	if err != nil {
		return err
	}
	defer sec.Leave()
	if err := s.writeConfig(sizes); err != nil {
		return err
	}
	obs, err := s.observe()
	if err != nil {
		return err
	}
	s.sizes = maps.Clone(sizes)
	for r, n := range sizes {
		s.ceiling[r] = max(n, obs.lockedCount[r])
	}
	return nil
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

// observe reads the token locks and the live holds. The caller is inside the section.
func (s *Sampler) observe() (observation, error) {
	obs := observation{at: time.Now(), locked: map[Token]bool{}, lockedCount: map[string]int{},
		inodes: map[Token][2]uint64{}}
	files, err := store.TokenFiles(s.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return obs, err
	}
	for _, f := range files {
		k := Token{f.Resource, f.Index}
		obs.inodes[k] = [2]uint64{f.Dev, f.Ino}
		locked, err := probeLocked(f.Path)
		if err != nil {
			return obs, err
		}
		if locked {
			obs.locked[k] = true
			obs.lockedCount[k.Res]++
		}
	}
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
		data, err := os.ReadFile(filepath.Join(s.dir, store.HoldsDir, en.Name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return obs, err
		}
		h, err := wire.DecodeHold(data)
		if err != nil {
			obs.malformed = append(obs.malformed, en.Name)
			continue
		}
		obs.holds = append(obs.holds, liveHold{Name: en.Name, Body: h})
	}
	return obs, nil
}

// probeLocked reports whether another open file description holds path's lock: one LOCK_NB
// attempt, released at once by closing.
func probeLocked(path string) (bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe %s: %w", path, err)
	}
	defer unix.Close(fd)
	err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe %s: %w", path, err)
	}
	return false, nil
}

// check returns the violations obs shows, by key, and moves the I3 bound. The caller holds mu.
func (s *Sampler) check(obs observation) map[string]Violation {
	found := map[string]Violation{}
	add := func(inv, key, format string, args ...any) {
		found[key] = Violation{Invariant: inv, Key: key, Msg: fmt.Sprintf(format, args...), At: obs.at}
	}

	// I1: a token file keeps its inode for ever (token files are never deleted), and no two
	// live holds list one token.
	for k, ino := range obs.inodes {
		if first, seen := s.inodes[k]; !seen {
			s.inodes[k] = ino
		} else if first != ino {
			add("I1", "I1 inode "+k.String(), "token %s changed inode from %v to %v", k, first, ino)
		}
	}
	listed := map[Token][]string{}
	for _, h := range obs.holds {
		for r, idxs := range h.Body.Grant {
			for _, i := range idxs {
				k := Token{r, i}
				listed[k] = append(listed[k], h.Name)
				// I2: a live hold lists exactly the tokens it locks.
				if !obs.locked[k] {
					add("I2", "I2 unlocked "+h.Name+" "+k.String(),
						"live hold %s lists token %s, which nothing locks", h.Name, k)
				}
			}
		}
	}
	for k, names := range listed {
		if len(names) > 1 {
			slices.Sort(names)
			add("I1", "I1 shared "+k.String(), "token %s is listed by %d live holds: %v", k, len(names), names)
		}
	}

	// I2: a locked token is listed by a live hold, or inside a token-only grant of the test.
	for k := range s.unlisted {
		if !obs.locked[k] || len(listed[k]) > 0 {
			delete(s.unlisted, k)
		}
	}
	for k := range obs.locked {
		if len(listed[k]) > 0 {
			continue
		}
		since, seen := s.unlisted[k]
		if !seen {
			since = obs.at
			s.unlisted[k] = since
		}
		if s.Exempt != nil && s.Exempt(k, obs.inodes[k], obs.at.Sub(since)) {
			continue
		}
		add("I2", "I2 unlisted "+k.String(), "token %s is locked and listed by no live hold", k)
	}

	// I3: the locked count of a resource, at every index, stays within the bound.
	for _, r := range slices.Sorted(maps.Keys(s.sizes)) {
		size, bound, n := s.sizes[r], s.ceiling[r], obs.lockedCount[r]
		if n > max(size, bound) {
			add("I3", "I3 "+r, "%d tokens of %s locked; size %d, bound %d", n, r, size, bound)
		}
		s.ceiling[r] = max(size, min(bound, n))
	}

	// I7: an ordinary admission grants at least min(min, size); a clamped one records the
	// requested min and holds fewer.
	for _, h := range obs.holds {
		if h.Body.Unbudgeted {
			add("fail-open", "fail-open "+h.Name, "hold %s is unbudgeted (a client failed open)", h.Name)
		}
		if s.Request == nil {
			continue
		}
		need, ok := s.Request(h.Body.Label)
		if !ok {
			continue
		}
		for _, r := range slices.Sorted(maps.Keys(need)) {
			rg, n := need[r], len(h.Body.Grant[r])
			cl, clamped := h.Body.Clamped[r]
			bad := false
			if clamped {
				bad = cl != rg.Min || n < 1 || n >= rg.Min
			} else {
				bad = n < rg.Min || n > rg.Max
			}
			if bad {
				add("I7", "I7 "+h.Name+" "+r, "hold %s grants %d of %s for %d..%d (clamped %v: %d)",
					h.Name, n, r, rg.Min, rg.Max, clamped, cl)
			}
		}
	}
	for _, name := range obs.malformed {
		add("I2", "malformed "+name, "live hold %s has a malformed body", name)
	}
	return found
}

// Leftovers returns, from one observation inside the section, the locked tokens and the live
// holds: after every client is gone both are empty.
func (s *Sampler) Leftovers(ctx context.Context) (locked []Token, holds []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, err := s.st.Enter(ctx)
	if err != nil {
		return nil, nil, err
	}
	obs, err := s.observe()
	sec.Leave()
	if err != nil {
		return nil, nil, err
	}
	for k := range obs.locked {
		locked = append(locked, k)
	}
	slices.SortFunc(locked, func(a, b Token) int { return strings.Compare(a.String(), b.String()) })
	for _, h := range obs.holds {
		holds = append(holds, h.Name)
	}
	return locked, holds, nil
}

// ---------------------------------------------------------------- the sampler's own test

// lockToken takes the lock of token k outside the section, as no conforming client does, and
// returns the descriptor that holds it. Closing the descriptor releases it.
func lockToken(t *testing.T, e Env, k Token) int {
	t.Helper()
	dir := filepath.Join(e.Dir, store.ResDir, k.Res)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(filepath.Join(dir, fmt.Sprint(k.Idx)), unix.O_RDONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("lock %s: %v", k, err)
	}
	return fd
}

// fakeHolds publishes, inside the section, one hold per entry of lists (label, then the cpu
// indices it lists), after taking take tokens of cpu at indices below below. The holds list
// what they are given, not what was taken: that is how a defective client is built. close
// releases everything.
func fakeHolds(t *testing.T, e Env, take, below int, lists map[string][]int) (close func()) {
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
	ts, got, err := sec.TakeTokens(map[string]int{"cpu": take}, map[string]int{"cpu": below})
	if err != nil || got["cpu"] != take {
		t.Fatalf("take %d tokens: got %v, %v", take, got, err)
	}
	var files []*os.File
	for _, label := range slices.Sorted(maps.Keys(lists)) {
		h := wire.Hold{Label: label, Grant: map[string][]int{"cpu": lists[label]}, StartNS: uint64(store.NowNS())}
		f, _, err := sec.PublishHold(h, os.Getpid(), store.NextSeq())
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if err := sec.BumpGen(); err != nil {
		t.Fatal(err)
	}
	return func() {
		for _, f := range files {
			f.Close()
		}
		ts.Close()
	}
}

// TestSamplerCatchesAViolation passes the sampler a valid history of real runs, then builds
// real violations of I1, I2, I3 and I7 in the state directory and checks that the sampler
// reports each one, and only it, once two samples saw it; a momentary one is not reported.
func TestSamplerCatchesAViolation(t *testing.T) {
	e := State(t)
	s := NewSampler(t, e, map[string]int{"cpu": 4})
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

	// A valid history: two runs hold, a third waits for more than is free, then each is
	// released in turn, with samples throughout.
	a := e.Run("v1.cli.c2_2", "--need", "cpu=2")
	e.WaitHold("v1.cli.c2_2", startWait)
	b := e.Run("v2.cli.c1_2", "--need", "cpu=1..2")
	e.WaitHold("v2.cli.c1_2", startWait)
	c := e.Run("v3.cli.c3_3", "--need", "cpu=3")
	e.WaitTicket("v3.cli.c3_3")
	expect("two holds and a waiter")
	e.ReleaseChild("v1.cli.c2_2")
	a.ExitCode(0)
	e.ReleaseChild("v2.cli.c1_2")
	b.ExitCode(0)
	e.WaitHold("v3.cli.c3_3", startWait)
	expect("the waiter admitted")
	e.ReleaseChild("v3.cli.c3_3")
	c.ExitCode(0)
	expect("everything released")

	t.Run("an unlisted token held across samples is reported (I2)", func(t *testing.T) {
		fd := lockToken(t, e, Token{"cpu", 3})
		expect("cpu/3 locked outside the section", "I2 unlisted cpu/3")
		unix.Close(fd)
		settle()
	})
	t.Run("a momentary unlisted lock is not reported", func(t *testing.T) {
		fd := lockToken(t, e, Token{"cpu", 2})
		if got := sample(); len(got) != 0 {
			t.Errorf("the sample that saw cpu/2 locked reported %v", got)
		}
		unix.Close(fd)
		if got := sample(); len(got) != 0 {
			t.Errorf("the sample after cpu/2 was released reported %v", got)
		}
	})
	t.Run("more tokens locked than the size is reported (I3)", func(t *testing.T) {
		release := fakeHolds(t, e, 6, 8, map[string][]int{"x.lib.c6_6": {0, 1, 2, 3, 4, 5}})
		expect("6 tokens held under size 4", "I3 cpu")
		release()
		settle()
	})
	t.Run("a token listed by two live holds is reported (I1)", func(t *testing.T) {
		release := fakeHolds(t, e, 1, 4, map[string][]int{"y.lib.c1_1": {0}, "z.lib.c1_1": {0}})
		expect("cpu/0 listed twice", "I1 shared cpu/0")
		release()
		settle()
	})
	t.Run("a grant below its minimum is reported (I7)", func(t *testing.T) {
		release := fakeHolds(t, e, 1, 4, map[string][]int{"w.lib.c3_3": {0}})
		expect("1 token granted for cpu=3", "I7 "+holdNamed(t, e, "w.lib.c3_3")+" cpu")
		release()
		settle()
	})
	t.Run("a live hold listing a token nobody locks is reported (I2)", func(t *testing.T) {
		release := fakeHolds(t, e, 1, 4, map[string][]int{"u.lib.c2_2": {0, 1}})
		expect("cpu/1 listed, not locked", "I2 unlocked "+holdNamed(t, e, "u.lib.c2_2")+" cpu/1")
		release()
		settle()
	})
	t.Run("a shrink leaves locks above the size without a violation", func(t *testing.T) {
		release := fakeHolds(t, e, 4, 4, map[string][]int{"r.lib.c4_4": {0, 1, 2, 3}})
		if err := s.Resize(ctx, map[string]int{"cpu": 2}); err != nil {
			t.Fatal(err)
		}
		expect("4 tokens held after a shrink to 2")
		release()
		settle()
		if err := s.Resize(ctx, map[string]int{"cpu": 4}); err != nil {
			t.Fatal(err)
		}
	})
}

// holdNamed returns the name of the hold labelled label.
func holdNamed(t *testing.T, e Env, label string) string {
	t.Helper()
	h, ok := e.Hold(label)
	if !ok {
		t.Fatalf("no hold labelled %s", label)
	}
	return h.Name
}
