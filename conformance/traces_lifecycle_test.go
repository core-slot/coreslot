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
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
)

// suspendFor is how long the suspend traces keep every waiter stopped: longer than the 10 s
// after which an observer calls an unchanged ticket STALLED.
const suspendFor = 15 * time.Second

// ---------------------------------------------------------------- helpers

// writeConfigAtomic replaces the configuration with body through a rename, so that no client
// ever reads it half written (a torn read is an invalid configuration, which fails open). The
// temporary lies outside the state directory, whose stray entries are a state error.
func writeConfigAtomic(e Env, body string) {
	e.t.Helper()
	tmp := filepath.Join(e.MarkDir, "config.toml.partial")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(e.Dir, store.ConfigFile)); err != nil {
		e.t.Fatal(err)
	}
}

// writeFormat replaces FORMAT with the line of format n, through a rename.
func writeFormat(e Env, n string) {
	e.t.Helper()
	tmp := filepath.Join(e.MarkDir, "FORMAT.partial")
	if err := os.WriteFile(tmp, []byte("coreslot-protocol "+n+"\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(e.Dir, store.FormatFile)); err != nil {
		e.t.Fatal(err)
	}
}

// cpuLocked is the number of locked cpu tokens, at every index, probed by the test itself
// rather than read through the library, so that a defect in the clients' counting cannot hide
// from it.
func cpuLocked(e Env) int {
	e.t.Helper()
	dir := filepath.Join(e.Dir, store.ResDir, "cpu")
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.t.Fatal(err)
	}
	n := 0
	for _, en := range entries {
		f, err := os.Open(filepath.Join(dir, en.Name()))
		if err != nil {
			e.t.Fatal(err)
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); errors.Is(err, unix.EWOULDBLOCK) {
			n++
		} else if err != nil {
			e.t.Fatal(err)
		}
		f.Close()
	}
	return n
}

// stableLocked is the locked cpu count once it persists across two samples 100 ms apart. A client's
// probe locks a free token for an instant (PROTOCOL.md section 6), so one sample taken while a
// waiter counts tokens may read high; a real lock persists.
func stableLocked(e Env) int {
	e.t.Helper()
	prev := cpuLocked(e)
	for range 20 {
		time.Sleep(100 * time.Millisecond)
		n := cpuLocked(e)
		if n == prev {
			return n
		}
		prev = n
	}
	e.t.Fatalf("the locked cpu count did not hold still across two samples 100 ms apart in 2 s (last %d)", prev)
	return 0
}

// indices are the cpu token indices the live hold labeled label lists. Call it only while no
// admission is in flight: a snapshot probes the tokens, and a probe can make an admitting
// client skip a token (PROTOCOL.md section 6), so exact indices are read once the commands
// started (WaitStarted reads marker files and probes nothing).
func indices(e Env, label string) []int {
	e.t.Helper()
	h, ok := e.Hold(label)
	if !ok {
		e.t.Fatalf("no live hold labelled %s", label)
	}
	return e.HoldBody(h.Name).Grant["cpu"]
}

func span(from, to int) []int {
	var s []int
	for i := from; i <= to; i++ {
		s = append(s, i)
	}
	return s
}

// lockedSampler samples the locked cpu count and the size every 100 ms through the library's
// Snapshot, until stop is called. A count above the size is a violation only when it persists
// across two consecutive samples: a client's probe locks a free token for an instant. It records
// how many samples it took and every Snapshot error.
type lockedSampler struct {
	stop       func()
	mu         sync.Mutex
	samples    int
	maxLocked  int // the largest count that persisted across two samples
	violations []string
	errs       []error
}

func sampleLocked(e Env) *lockedSampler {
	s := &lockedSampler{}
	quit, finished := make(chan struct{}), make(chan struct{})
	c := e.Client()
	go func() {
		defer close(finished)
		prevOver, prev := false, -1
		for {
			select {
			case <-quit:
				return
			case <-time.After(100 * time.Millisecond):
			}
			rep, err := c.Snapshot()
			s.mu.Lock()
			if err != nil {
				s.errs = append(s.errs, err)
				prevOver, prev = false, -1
				s.mu.Unlock()
				continue
			}
			r := rep.Resources["cpu"]
			s.samples++
			if prev >= 0 {
				s.maxLocked = max(s.maxLocked, min(prev, r.Locked))
			}
			over := r.Locked > r.Size
			if over && prevOver {
				s.violations = append(s.violations, fmt.Sprintf("%s: %d locked, size %d (and above it 100 ms earlier)",
					time.Now().Format(time.StampMicro), r.Locked, r.Size))
			}
			prevOver, prev = over, r.Locked
			s.mu.Unlock()
		}
	}()
	var once sync.Once
	s.stop = func() { once.Do(func() { close(quit); <-finished }) }
	return s
}

// check stops the sampler and fails the test on a persisting violation, on any Snapshot error,
// or when it took fewer than minSamples samples.
func (s *lockedSampler) check(t *testing.T, minSamples int) {
	t.Helper()
	s.stop()
	if len(s.errs) > 0 {
		t.Errorf("the sampler's Snapshot failed %d times; first: %v", len(s.errs), s.errs[0])
	}
	if s.samples < minSamples {
		t.Errorf("the sampler took %d samples, want at least %d", s.samples, minSamples)
	}
	if len(s.violations) > 0 {
		t.Errorf("locked tokens exceeded the size: %v", s.violations)
	}
	t.Logf("the sampler took %d samples; the most locked tokens that persisted: %d", s.samples, s.maxLocked)
}

// ---------------------------------------------------------------- wall-clock steps

// A wall-clock step of every process changes nothing, because freshness is observed on each
// observer's monotonic clock and never computed from an mtime. The head A needs both tokens
// and B, behind it, needs the free one. Twice, A is caught right after a refresh, every
// process's wall clock steps by an hour (backward, or forward), and A stays live but silent for
// 3 s. B must not pass it. The first time A then goes on refreshing on the stepped clock, and B
// still waits behind it for longer than the stall threshold. The second time A is stopped: B
// passes it within the bound, but never before 10 s after A's last refresh. A forward step is
// also a refresh gap above 10 s on every waiter's wall clock: each bumps gen once (the resume
// bump's wall-clock arm); a backward step bumps nothing.
//
// Mutation: judge STALLED by the observer's wall clock (wall clock minus the last mtime read
// above 10 s) in Observer.Stalled instead of observing changes. Forward reddens at once (B passes
// the live, silent A); backward reddens at the bound (A's mtime lies in B's future, so A never
// stalls).
// Mutation: drop the wall-clock arm of the resume bump (refresh checks the monotonic gap only):
// the forward step bumps nothing.
//
// -short runs the backward step only.
func TestClockStepThenHeadStops(t *testing.T) {
	for _, c := range []struct {
		name string
		step time.Duration
	}{
		{"backward", -time.Hour},
		{"forward", time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			if testing.Short() && c.step > 0 {
				t.Skip("-short: the forward step is not run")
			}
			e := State(t)
			e.WriteConfig(cpuConfig(2))
			x := e.Run("X", "--need", "cpu=1")
			e.WaitStarted("X")

			a := e.Pausing("admit.before-enter", "ticket.refreshed").Run("A", "--need", "cpu=2")
			defer a.AutoRelease("admit.before-enter")()
			autoRefresh := a.AutoRelease("ticket.refreshed")
			defer func() { autoRefresh() }()
			tkA := e.WaitTicket("A")
			b := e.Run("B", "--need", "cpu=1")
			e.Eventually(startWait, "B is queued", func() bool {
				tk, queued := e.Ticket("B")
				_, held := e.Hold("B")
				return queued && !tk.Dead || held
			})
			noHold(t, e, "B", "B passed the live head A")
			waitRefreshed(e, "A", tkA.Refreshed.UnixNano())

			// catchRefreshed holds A just after a refresh; it returns that refresh's mtime.
			catchRefreshed := func() int64 {
				t.Helper()
				autoRefresh()
				a.WaitReached("ticket.refreshed")
				tk, ok := e.Ticket("A")
				if !ok || tk.Dead {
					t.Fatal("A, held after a refresh, has no live ticket")
				}
				return tk.Refreshed.UnixNano()
			}

			// Phase 1: a step while A is live.
			catchRefreshed()
			e.SectionLeft()
			gen := e.Status().Gen
			e.StepClock(c.step)
			time.Sleep(3 * time.Second)
			noHold(t, e, "B", "B passed the live head A right after the clock stepped")
			a.Release("ticket.refreshed")
			autoRefresh = a.AutoRelease("ticket.refreshed")
			time.Sleep(12 * time.Second)
			noHold(t, e, "B", "B passed the live head A, refreshing on the stepped clock")
			want := gen
			if c.step > 0 {
				want = gen + 2 // A and B
			}
			e.SectionLeft()
			if got := e.Status().Gen; got != want {
				t.Errorf("gen went from %d to %d across the step, want %d", gen, got, want)
			}

			// Phase 2: A refreshes, the clock steps again, and A stops.
			mtime := catchRefreshed()
			lastRefresh := time.Unix(0, mtime-int64(c.step)) // A's clock was stepped once
			e.StepClock(2 * c.step)
			time.Sleep(3 * time.Second)
			noHold(t, e, "B", "B passed the live head A right after the clock stepped")
			a.Stop()
			stoppedAt := time.Now()
			a.Release("ticket.refreshed")
			autoRefresh = a.AutoRelease("ticket.refreshed")
			sectionFreeOf(t, e, a.PID())
			hb := e.WaitHold("B", stalledHeadBound+startWait)
			at := admittedAt(hb).Add(-2 * c.step) // B's start_ns is on its stepped clock
			if earliest := lastRefresh.Add(state.StallAfter); at.Before(earliest) {
				t.Fatalf("B was admitted at %v, %v after A's last refresh: before A could have been STALLED for 10 s",
					at, at.Sub(lastRefresh))
			}
			if d := at.Sub(stoppedAt); d > stalledHeadBound {
				t.Fatalf("B was admitted %v after the head stopped, bound %v (load: %s)", d, stalledHeadBound, uptime())
			} else {
				t.Logf("B was admitted %v after the head stopped, %v after its last refresh", d, at.Sub(lastRefresh))
			}

			a.Cont()
			e.ReleaseChild("B")
			b.ExitCode(0)
			e.ReleaseChild("X")
			x.ExitCode(0)
			e.WaitStarted("A")
			e.ReleaseChild("A")
			a.ExitCode(0)
		})
	}
}

// ---------------------------------------------------------------- suspend

// Every waiter is stopped for 15 s at once, as a laptop sleep stops them, and continued. Each
// waiter then refreshes and the queue resumes in order: once every waiter's refresh is
// observable, a token that the head cannot use but a later waiter could is not given to the
// later waiter (no backfill behind a live head), and the head is admitted first when its
// minimum frees up.
//
// Then the order holds from the resumption itself, before any refresh is observable. The head
// W3 needs two tokens and W4, behind it, needs one; both stop and one token frees up meanwhile.
// W3 stops first and W4 2 s later, so W4 last saw W3's final mtime before its own stop. W4 is
// continued first and W3 about 3 s later: W4 has watched W3 unchanged for at most about 7 s (W3's
// last refresh may precede its stop by up to 2 s, W4 watches 2 s more before its own stop, and
// 3 s after its continuation), leaving about 3 s below the 10 s of a stall, since the 15 s in
// which W4 itself made no pass were not watched (PROTOCOL.md section 10.2). So W3 is not STALLED
// for it and W4 must not take the token. Once the rest frees up, W3 is admitted
// before W4.
//
// Last, with capacity freed while every waiter is stopped, both remaining waiters (each fits)
// are admitted within the I5 bound of the resumption; which goes first is not asserted, as they
// are continued together.
//
// Mutation: a STALLED verdict sticks (Observer.Observe ignores a changed mtime once the ticket
// is stalled): after the resumption W2 keeps passing over the live head W1 and takes the token.
// Mutation: count the observer's own absence as watched (Observer.Pass never adds a gap above
// MaxPassGap to the unwatched time): W4 finds W3 STALLED as soon as it is continued and takes
// the free token in the second phase.
// Mutation: a resumed waiter waits too long before its next election (refresh, on a refresh gap
// above 10 s, also defers the next election by 25 s): in the third phase the waiter that was
// not caught mid-attempt is admitted only after the bound.
//
// -short skips the third phase.
func TestSuspendAllWaiters(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(3))
	x1 := e.Run("X1", "--need", "cpu=1")
	e.WaitHold("X1", startWait)
	x2 := e.Run("X2", "--need", "cpu=2")
	e.WaitHold("X2", startWait)

	w1, autoW1 := startStoppableHead(e, "W1", "--need", "cpu=2")
	e.WaitTicket("W1")
	w2 := e.Run("W2", "--need", "cpu=1")
	e.WaitTicket("W2")
	w3, autoW3 := startStoppableHead(e, "W3", "--need", "cpu=2")
	e.WaitTicket("W3")
	w4 := e.Run("W4", "--need", "cpu=1")
	e.WaitTicket("W4")

	// Every waiter stops outside the section: W2-W4 wait behind the live head W1 and never enter
	// it once their enqueue sections have ended; W1 is caught before an admission attempt.
	e.SectionLeft()
	for _, p := range []*Proc{w4, w2} {
		p.Stop()
		sectionFreeOf(t, e, p.PID())
	}
	autoW3()
	w3.Stop()
	sectionFreeOf(t, e, w3.PID())
	stoppedAt, _ := stopOutsideSection(e, w1, autoW1)
	before := map[string]int64{}
	for _, name := range []string{"W1", "W2", "W3", "W4"} {
		tk, _ := e.Ticket(name)
		before[name] = tk.Refreshed.UnixNano()
	}
	time.Sleep(time.Until(stoppedAt.Add(suspendFor)))
	for _, p := range []*Proc{w4, w3, w2, w1} {
		p.Cont()
	}
	defer w1.AutoRelease("admit.before-enter")()
	autoW3 = w3.AutoRelease("admit.before-enter")
	defer func() { autoW3() }()

	// Phase 1: no capacity was freed. Once every refresh can have been observed, one token frees
	// up: W2 and W4 could use it (W3 needs two), the head W1 cannot, and nobody may take it.
	for name, mtime := range before {
		waitRefreshed(e, name, mtime)
	}
	time.Sleep(observeWait)
	e.ReleaseChild("X1")
	x1.ExitCode(0)
	time.Sleep(observeWait)
	for _, name := range []string{"W2", "W3", "W4"} {
		noHold(t, e, name, name+" was admitted past the live head W1 after the resumption")
	}
	e.ReleaseChild("X2")
	x2.ExitCode(0)
	h1 := e.WaitHold("W1", startWait)
	h2 := e.WaitHold("W2", startWait)
	if !admittedAt(h1).Before(admittedAt(h2)) {
		t.Errorf("W2 (admitted %v) went before W1 (admitted %v)", admittedAt(h2), admittedAt(h1))
	}
	noHold(t, e, "W3", "W3 was admitted with no token free")

	// Phase 2: the head W3 needs two tokens, W4 behind it one. W3 stops first, and W4 once its
	// elections (one per second) can have observed W3's last refresh, so that W3's mtime does not
	// change between W4's last pass before the suspension and its first pass after it. Meanwhile
	// W2 finishes, freeing one token. W4 is continued first and W3 resumptionGap later.
	const resumptionGap = 3 * time.Second
	stoppedAt, mtimeW3 := stopOutsideSection(e, w3, autoW3)
	autoW3 = w3.AutoRelease("admit.before-enter")
	time.Sleep(2 * time.Second)
	e.SectionLeft()
	w4.Stop()
	sectionFreeOf(t, e, w4.PID())
	if tk, ok := e.Ticket("W3"); !ok || tk.Refreshed.UnixNano() != mtimeW3 {
		t.Fatalf("the stopped W3's ticket changed or went away while W4 watched it (%v)", ok)
	}
	e.ReleaseChild("W2")
	w2.ExitCode(0)
	time.Sleep(time.Until(stoppedAt.Add(suspendFor)))
	w4.Cont()
	continuedW4 := time.Now()
	for time.Since(continuedW4) < resumptionGap {
		noHold(t, e, "W4", "W4 took the free token from the stopped head W3 right after its own resumption")
		time.Sleep(pollStep)
	}
	w3.Cont()
	t.Logf("W3 continued %v after W4", time.Since(continuedW4))
	waitRefreshed(e, "W3", mtimeW3)
	time.Sleep(observeWait)
	noHold(t, e, "W4", "W4 was admitted past the live head W3, which needs two tokens with one free")
	noHold(t, e, "W3", "W3 was admitted with one token free")
	e.ReleaseChild("W1")
	w1.ExitCode(0)
	h3 := e.WaitHold("W3", startWait)
	h4 := e.WaitHold("W4", startWait)
	if !admittedAt(h3).Before(admittedAt(h4)) {
		t.Errorf("W4 (admitted %v) went before W3 (admitted %v)", admittedAt(h4), admittedAt(h3))
	}

	if testing.Short() {
		t.Log("-short: the third phase (capacity freed during a common suspension) is not run")
		e.ReleaseChild("W3")
		e.ReleaseChild("W4")
		w3.ExitCode(0)
		w4.ExitCode(0)
		return
	}

	// Phase 3: W3 and W4 hold every token. W5 is the head and W6 waits behind it; both stop and
	// meanwhile W3 and W4 finish, freeing every token.
	w5, autoW5 := startStoppableHead(e, "W5", "--need", "cpu=1")
	e.WaitTicket("W5")
	w6 := e.Run("W6", "--need", "cpu=1")
	e.WaitTicket("W6")
	e.SectionLeft()
	w6.Stop()
	sectionFreeOf(t, e, w6.PID())
	stoppedAt, _ = stopOutsideSection(e, w5, autoW5)
	defer w5.AutoRelease("admit.before-enter")()
	e.ReleaseChild("W3")
	e.ReleaseChild("W4")
	w3.ExitCode(0)
	w4.ExitCode(0)
	time.Sleep(time.Until(stoppedAt.Add(suspendFor)))
	w6.Cont()
	w5.Cont()
	resumedAt := time.Now()
	var first coreslot.HoldInfo
	e.Eventually(stalledHeadBound+startWait, "a waiter is admitted after the resumption", func() bool {
		for _, name := range []string{"W5", "W6"} {
			if h, ok := e.Hold(name); ok {
				first = h
				return true
			}
		}
		return false
	})
	if d := admittedAt(first).Sub(resumedAt); d > stalledHeadBound {
		t.Errorf("%s was admitted %v after the resumption, bound %v (load: %s)", first.Label, d, stalledHeadBound, uptime())
	} else {
		t.Logf("%s was admitted %v after the resumption", first.Label, d)
	}
	// Both fit: whichever goes first, the other is then the head with its minimum free (I5), so
	// both are admitted within the bound of the resumption.
	h5 := e.WaitHold("W5", stalledHeadBound+startWait)
	h6 := e.WaitHold("W6", stalledHeadBound+startWait)
	for _, h := range []coreslot.HoldInfo{h5, h6} {
		if d := admittedAt(h).Sub(resumedAt); d > stalledHeadBound {
			t.Errorf("%s was admitted %v after the resumption, bound %v (load: %s)", h.Label, d, stalledHeadBound, uptime())
		}
	}
	t.Logf("W5 admitted %v after the resumption, W6 %v", admittedAt(h5).Sub(resumedAt), admittedAt(h6).Sub(resumedAt))
	e.ReleaseChild("W5")
	e.ReleaseChild("W6")
	w5.ExitCode(0)
	w6.ExitCode(0)
}

// ---------------------------------------------------------------- shrink

// A shrink from 14 to 12 while holds sit on indices 10-13 (I3): locked tokens are counted at
// every index, so the reduced budget is 12 minus all four, adoption takes only what is free,
// and no admission brings the locked count above 12. Releasing the holds above the new size
// frees capacity again (partial release); growing back to 14 lets a request use 12 and 13.
//
// Mutation: count locked tokens only at indices below the size (in store.countLocked, skip
// indices from 12 on): the adoption and R1 then take 10 between them and 14 tokens are locked
// under a size of 12.
func TestShrinkWithHighIndexHolds(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(14))
	l := e.Run("L", "--need", "cpu=10")
	e.WaitStarted("L")
	h1 := e.Run("H1", "--need", "cpu=2")
	e.WaitStarted("H1")
	h2 := e.Run("H2", "--need", "cpu=2")
	e.WaitStarted("H2")
	if got := indices(e, "H1"); !slices.Equal(got, []int{10, 11}) {
		t.Fatalf("H1 holds %v, want 10-11", got)
	}
	if got := indices(e, "H2"); !slices.Equal(got, []int{12, 13}) {
		t.Fatalf("H2 holds %v, want 12-13", got)
	}
	e.ReleaseChild("L")
	l.ExitCode(0)

	writeConfigAtomic(e, cpuConfig(12)+"# shrunk\n")
	if r := e.Status().Resources["cpu"]; r.Size != 12 || stableLocked(e) != 4 {
		t.Fatalf("after the shrink: size %d, %d tokens locked; want 12 and 4", r.Size, stableLocked(e))
	}

	// Adoption during the reduced period takes from what is free.
	c := e.Client()
	ga, err := c.Adopt(context.Background(), coreslot.Request{Label: "adopt-a", Need: map[string]coreslot.Range{"cpu": {Min: 2, Max: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ga.Close()
	if ga.Granted()["cpu"] != 3 || len(ga.Shortfall()) != 0 || !slices.Equal(indices(e, "adopt-a"), span(0, 2)) {
		t.Fatalf("adoption: grant %v, shortfall %v, indices %v; want 3 at 0-2", ga.Granted(), ga.Shortfall(), indices(e, "adopt-a"))
	}

	// A queued request may take at most what is left below the size: 5, from 3-7.
	r1 := e.Run("R1", "--need", "cpu=1..12")
	e.WaitStarted("R1")
	if got := indices(e, "R1"); !slices.Equal(got, span(3, 7)) {
		t.Errorf("R1 holds %v, want 3-7", got)
	}
	if got := stableLocked(e); got != 12 {
		t.Errorf("%d tokens locked under a size of 12", got)
	}

	// Nothing is free: an adoption takes nothing and reports its shortfall; a request waits.
	gb, err := c.Adopt(context.Background(), coreslot.Request{Label: "adopt-b", Need: map[string]coreslot.Range{"cpu": {Min: 1, Max: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gb.Close()
	if gb.Granted()["cpu"] != 0 || gb.Shortfall()["cpu"] != 1 {
		t.Errorf("adoption with nothing free: grant %v, shortfall %v; want none and a shortfall of 1", gb.Granted(), gb.Shortfall())
	}
	r2 := e.Run("R2", "--need", "cpu=1")
	e.WaitTicket("R2")
	time.Sleep(observeWait)
	noHold(t, e, "R2", "R2 was admitted with 12 tokens locked under a size of 12")
	if got := stableLocked(e); got != 12 {
		t.Errorf("%d tokens locked under a size of 12", got)
	}

	// Partial release: the holds above the new size go, and R2 fits at index 8.
	e.ReleaseChild("H2")
	h2.ExitCode(0)
	e.WaitStarted("R2")
	if got := indices(e, "R2"); !slices.Equal(got, []int{8}) {
		t.Errorf("R2 holds %v, want 8", got)
	}

	// Growth back to 14: 9, 12 and 13 are free (10 and 11 are H1's).
	writeConfigAtomic(e, cpuConfig(14))
	r3 := e.Run("R3", "--need", "cpu=3")
	e.WaitStarted("R3")
	if got := indices(e, "R3"); !slices.Equal(got, []int{9, 12, 13}) {
		t.Errorf("R3 holds %v, want 9, 12 and 13", got)
	}
	if got := stableLocked(e); got != 14 {
		t.Errorf("%d tokens locked, want 14", got)
	}
	for _, p := range []*Proc{h1, r1, r2, r3} {
		e.ReleaseChild(p.name)
		p.ExitCode(0)
	}
}

// ---------------------------------------------------------------- unknown format

// layout reads every file of the state directory except the token files, the run log
// included: relative path -> content.
func layout(t *testing.T, e Env) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(e.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(e.Dir, path)
		if d.IsDir() {
			if rel == store.ResDir {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// FORMAT 2, a format this client does not implement, is written while one run holds and two
// wait. The waiters switch to token-only mode as they next enter the section: the head
// withdraws (its ticket is dead) and the one behind it takes its token outside the queue. A run
// that arrives afterwards never queues. Locked tokens never exceed the size, and the token-only
// clients write nothing: no ticket, hold, gen bump, admit.lock body, degraded record or run-log
// line. The format-1 holder's release, whose section finds FORMAT 2, leaves its hold in place
// rather than removing it under a format it does not implement, and the holder writes no
// run-log line either: it re-reads FORMAT just before the append (PROTOCOL.md section 20). The
// run log never comes to exist.
//
// Mutation: a token-only run appends its run-log line (runChild given the store's log for a
// token-only grant): lines for W1, W2 and N appear.
// Mutation: skip the re-read of FORMAT before the append (appendRunLog appends after the
// rotation check alone): H's line appears.
// Mutation: token-only admission is not bounded by the size (tokenOnlyAttempt skips the free
// check and takes at indices up to the size plus 4): W1 takes indices 3 and 4 while H and W2
// hold the other three, and the sampler sees 5 locked under a size of 3.
func TestUnknownFormatTokenOnly(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(3))
	h := e.Run("H", "--need", "cpu=2")
	hh := e.WaitHold("H", startWait)
	w1 := e.Run("W1", "--need", "cpu=2")
	e.WaitTicket("W1")
	w2 := e.Run("W2", "--need", "cpu=1")
	e.WaitTicket("W2")
	sampler := sampleLocked(e)
	defer sampler.stop()

	// FORMAT 2 is written while the test sits in the section, so no section is in flight.
	marker := filepath.Join(e.MarkDir, "section")
	s := e.StartChild("section", e.Dir, marker)
	e.Eventually(startWait, "the helper enters the section", func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})
	writeFormat(e, "2")
	before := layout(t, e)
	gen := e.Status().Gen
	if err := os.WriteFile(marker+".release", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.ExitCode(0)

	// W2 runs, token-only, on the free token; W1 (needing two) waits token-only, its ticket dead.
	r2 := e.WaitStarted("W2")
	if r2.Env["CORESLOT_UNBUDGETED"] != "format" || r2.Env["CORESLOT_GRANTED_CPU"] != "1" ||
		r2.Env["CORESLOT_HELD_FD"] != "" || !slices.Contains([]string{"4", "4-4"}, r2.Env["CORESLOT_TOKEN_FDS"]) {
		t.Errorf("token-only command environment %v", r2.Env)
	}
	if _, ok := r2.FDs[3]; ok {
		t.Errorf("the token-only command has fd 3 open: %v", r2.FDs)
	}
	if got := stableLocked(e); got != 3 {
		t.Errorf("%d tokens locked, want 3 (H's two and W2's one)", got)
	}
	e.Eventually(startWait, "W1's ticket is dead", func() bool {
		tk, ok := e.Ticket("W1")
		return ok && tk.Dead
	})

	// A run arriving under FORMAT 2 waits token-only, outside the queue.
	n := e.Run("N", "--need", "cpu=1")
	time.Sleep(observeWait)
	if _, ok := e.Ticket("N"); ok {
		t.Error("N published a ticket under FORMAT 2")
	}
	for _, name := range []string{"W1", "N"} {
		if _, ok := e.Started(name); ok {
			t.Errorf("%s ran with no token free", name)
		}
	}

	// H finishes and W2 too: W1 and N, both token-only, take what frees up.
	e.ReleaseChild("H")
	h.ExitCode(0)
	e.ReleaseChild("W2")
	w2.ExitCode(0)
	r1 := e.WaitStarted("W1")
	rn := e.WaitStarted("N")
	for name, r := range map[string]ChildReport{"W1": r1, "N": rn} {
		if r.Env["CORESLOT_UNBUDGETED"] != "format" {
			t.Errorf("%s's command environment %v", name, r.Env)
		}
	}
	e.ReleaseChild("W1")
	e.ReleaseChild("N")
	w1.ExitCode(0)
	n.ExitCode(0)
	sampler.check(t, 20)
	for _, p := range []*Proc{w1, w2, n} {
		if !strings.Contains(p.Stderr(), "token-only") {
			t.Errorf("%s printed no token-only diagnostic: %q", p.name, p.Stderr())
		}
	}

	after := layout(t, e)
	if !mapsEqual(before, after) {
		t.Errorf("the state directory changed under FORMAT 2:\nbefore %q\nafter  %q", sortedKeys(before), sortedKeys(after))
		for k, v := range after {
			if before[k] != v {
				t.Errorf("%s: %q -> %q", k, before[k], v)
			}
		}
	}
	if _, ok := before[filepath.Join(store.HoldsDir, hh.Name)]; !ok {
		t.Errorf("H's hold %s was not in the layout", hh.Name)
	}
	if got := e.Status().Gen; got != gen {
		t.Errorf("gen went from %d to %d under FORMAT 2", gen, got)
	}
	if _, err := os.Stat(filepath.Join(e.Dir, store.LogDir, store.RunLog)); err == nil {
		t.Errorf("a run-log line was written under FORMAT 2: %v", runLog(t, e))
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Error(err)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- cancellation

// A cancel request published before the owner's admitting section wins even though the owner
// already won its election and capacity is free: admission step 1 finds it, nothing runs and
// nothing is left. One made while the owner is inside the section that admits it waits for the
// section, finds no ticket, and has no effect: cancellation never revokes a grant. The CLI's
// cancel waits for the section for 1 s at most: an owner that stays inside longer makes it
// refuse, naming the holder, with nothing published, and the owner is admitted as if no cancel
// had been made.
//
// Mutations: skip admission step 1's cancel check (the "before" run is admitted and its command
// runs); publish a cancel request without checking that the ticket exists (the "after" cancel
// succeeds and leaves a request behind); wait for the section without a bound ("after, past the
// bound": cancel does not exit while V is paused).
func TestCancelBeforeAndAfterAdmittingSection(t *testing.T) {
	t.Run("before", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(cpuConfig(1))
		x := e.Run("X", "--need", "cpu=1")
		e.WaitHold("X", startWait)
		w := e.Pausing("admit.before-enter").Run("W", "--need", "cpu=1")
		autoW := w.AutoRelease("admit.before-enter")
		tk := e.WaitTicket("W")
		// W is caught after an election, before a section; X then frees the token, so that
		// section would admit it.
		autoW()
		w.WaitReached("admit.before-enter")
		e.ReleaseChild("X")
		x.ExitCode(0)
		if got := e.Status().Resources["cpu"].Available; got != 1 {
			t.Fatalf("%d cpu free, want 1: W's attempt would not admit it", got)
		}
		e.Start("cancel", tk.ID).ExitCode(0)
		w.Release("admit.before-enter")
		defer w.AutoRelease("admit.before-enter")()
		e.Eventually(exitWait, "W ends or its command starts", func() bool {
			_, ok := e.Started("W")
			return ok || w.Exited()
		})
		if _, ok := e.Started("W"); ok {
			t.Fatal("W was admitted and its command runs: the cancel request published before its admitting section did not win")
		}
		w.ExitCode(exitRefusal)
		rep := e.Status()
		if len(rep.Holds) != 0 || len(rep.DeadHolds) != 0 || len(rep.Tickets) != 0 || len(rep.CancelRequests) != 0 ||
			rep.Resources["cpu"].Locked != 0 {
			t.Errorf("after the cancellation: holds %v, dead %v, tickets %v, cancels %v, locked %d",
				rep.Holds, rep.DeadHolds, rep.Tickets, rep.CancelRequests, rep.Resources["cpu"].Locked)
		}
	})

	t.Run("after", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(cpuConfig(1))
		v := e.Pausing("publish.after-create").Run("V", "--need", "cpu=1")
		// A hold is published only inside the section that admits it, so V is paused in it.
		v.CatchPublication("publish.after-create", store.HoldsDir)
		tk, ok := e.Ticket("V")
		if !ok {
			t.Fatal("V has no ticket inside its admitting section")
		}
		// The canceller starts while V holds its admitting section, and V leaves it well within
		// cancel's 1 s bound, so whenever cancel enters the section V's admission is complete and
		// the ticket gone. (Whether it has tried to enter before V leaves depends on scheduling;
		// the outcome does not.)
		c := e.Start("cancel", tk.ID)
		time.Sleep(200 * time.Millisecond)
		if c.Exited() {
			t.Fatalf("cancel exited %d while V held the section; stderr:\n%s", c.Wait(), c.Stderr())
		}
		v.Release("publish.after-create")
		defer v.AutoRelease("publish.after-create")()
		c.ExitCode(exitRefusal)
		if !strings.Contains(c.Stderr(), "no waiting ticket") {
			t.Errorf("cancel stderr %q, want no waiting ticket (load: %s)", c.Stderr(), uptime())
		}
		e.WaitStarted("V")
		e.WaitHold("V", startWait)
		if rep := e.Status(); len(rep.CancelRequests) != 0 {
			t.Errorf("a cancel request was left for the admitted ticket: %v", rep.CancelRequests)
		}
		e.ReleaseChild("V")
		v.ExitCode(0)
	})

	t.Run("after, past the bound", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(cpuConfig(1))
		v := e.Pausing("publish.after-create").Run("V", "--need", "cpu=1")
		v.CatchPublication("publish.after-create", store.HoldsDir)
		tk, ok := e.Ticket("V")
		if !ok {
			t.Fatal("V has no ticket inside its admitting section")
		}
		// V stays inside its admitting section until cancel has given up: cancel refuses, names
		// V as the holder, and publishes nothing.
		c := e.Start("cancel", tk.ID)
		c.ExitCode(exitRefusal)
		holder := fmt.Sprintf("held by pid %d", v.PID())
		if !strings.Contains(c.Stderr(), "stayed held") || !strings.Contains(c.Stderr(), holder) ||
			!strings.Contains(c.Stderr(), "no cancel request was published") {
			t.Errorf("cancel stderr %q, want a refusal naming %q", c.Stderr(), holder)
		}
		if rep := e.Status(); len(rep.CancelRequests) != 0 {
			t.Errorf("a refused cancel published a request: %v", rep.CancelRequests)
		}
		v.Release("publish.after-create")
		defer v.AutoRelease("publish.after-create")()
		e.WaitStarted("V")
		e.WaitHold("V", startWait)
		e.ReleaseChild("V")
		v.ExitCode(0)
	})
}

// TestShortRunPassesMemoryWaiters: two runs wait at the front of the queue for memory another run
// holds, and a 50 ms run third in the queue, naming cpu alone, is admitted at once behind every
// minimum ahead (PROTOCOL.md section 10.3): its extra pass leaves the cpu both waiters need, so
// it is granted 2 of the 4 cpus. Once the memory is released the head is admitted as it would be
// without the rule, and the run behind it too. Under [memory] live = true the short run, which
// names no mem, is admitted the same way, though its memory is in no reservation: that caveat is
// accepted and pinned here. Every client runs with no live headroom, so the waiters wait for the
// static size.
//
// Mutations: no passing outside the head leaves C queued while X holds the memory; Plan in place
// of state.PlanBehind grants C all 4 cpus.
func TestShortRunPassesMemoryWaiters(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live = %v", live), func(t *testing.T) {
			e := State(t)
			e.WriteConfig(fmt.Sprintf("[resources]\ncpu = 4\nmem = \"1G\"\n\n[memory]\nlive = %v\n", live))
			// run starts `coreslot run` of argv (a helper child that holds until released when
			// argv is nil) needing each of needs.
			run := func(name string, needs []string, argv ...string) *Proc {
				if argv == nil {
					argv = []string{bins.child, "hold", e.MarkDir, name}
				}
				args := []string{"/usr/bin/env", "CORESLOT_TESTHOOK_HEADROOM_MIB=0", bins.coreslot, "run", "--label", name}
				for _, n := range needs {
					args = append(args, "--need", n)
				}
				p := e.StartCommand(append(append(args, "--"), argv...)...)
				p.name = name
				return p
			}
			waiter := []string{"cpu=1", "mem=512M"}
			x := run("X", []string{"mem=1G"})
			e.WaitHold("X", startWait)
			a := run("A", waiter)
			e.WaitTicket("A")
			b := run("B", waiter)
			e.WaitTicket("B")

			start := time.Now()
			c := run("C", []string{"cpu=1..4"}, "sh", "-c", `echo $CORESLOT_GRANTED_CPU; sleep 0.05`)
			c.ExitCode(0)
			t.Logf("C ran %v after it started, behind A and B", time.Since(start))
			if got := strings.TrimSpace(c.Stdout()); got != "2" {
				t.Fatalf("C was granted %s cpus, want 2: the cpu minimums of A and B stay free", got)
			}
			noHold(t, e, "A", "X still holds every mem token")
			noHold(t, e, "B", "X still holds every mem token")

			e.ReleaseChild("X")
			x.ExitCode(0)
			e.WaitHold("A", startWait)
			e.WaitHold("B", startWait)
			for name, p := range map[string]*Proc{"A": a, "B": b} {
				e.ReleaseChild(name)
				p.ExitCode(0)
			}
		})
	}
}
