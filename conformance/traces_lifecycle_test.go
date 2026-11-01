//go:build coreslot_testhooks && unix

package conformance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
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

// memReserved is what the live holds reserve, in units of 128 MiB, each hold probed and read by
// the test itself rather than through the engine, so that a defect in the clients' counting
// cannot hide from it.
func memReserved(e Env) int {
	e.t.Helper()
	dir := filepath.Join(e.Dir, store.HoldsDir)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.t.Fatal(err)
	}
	var mib int64
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".") {
			continue // a temporary file
		}
		f, err := os.Open(filepath.Join(dir, en.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			e.t.Fatal(err)
		}
		data, rerr := io.ReadAll(f)
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			h, derr := wire.DecodeHold(data)
			if rerr != nil || derr != nil {
				e.t.Fatalf("live hold %s: %v, %v", en.Name(), rerr, derr)
			}
			mib += h.NeedMiB
		} else if err != nil {
			e.t.Fatal(err)
		}
	}
	return int(mib / 128)
}

// stableReserved is memReserved once it persists across two samples 100 ms apart. A client's
// probe locks a dead hold for an instant (PROTOCOL.md section 6), so one sample taken while a
// waiter probes may read high; a real reservation persists.
func stableReserved(e Env) int {
	e.t.Helper()
	prev := memReserved(e)
	for range 20 {
		time.Sleep(100 * time.Millisecond)
		n := memReserved(e)
		if n == prev {
			return n
		}
		prev = n
	}
	e.t.Fatalf("the reservations did not hold still across two samples 100 ms apart in 2 s (last %d)", prev)
	return 0
}

// reservedSampler samples the reservations and the size every 100 ms through the engine's
// Snapshot, until stop is called. Reservations above the size are a violation only when they
// persist across two consecutive samples: a client's probe makes a dead hold look live for an
// instant. It records how many samples it took and every Snapshot error.
type reservedSampler struct {
	stop        func()
	mu          sync.Mutex
	samples     int
	maxReserved int64 // the most reserved that persisted across two samples, in MiB
	violations  []string
	errs        []error
}

func sampleReserved(e Env) *reservedSampler {
	s := &reservedSampler{}
	quit, finished := make(chan struct{}), make(chan struct{})
	c := e.Engine()
	go func() {
		defer close(finished)
		prevOver, prev := false, int64(-1)
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
			r := rep.Mem
			s.samples++
			if prev >= 0 {
				s.maxReserved = max(s.maxReserved, min(prev, r.ReservedMiB))
			}
			over := r.ReservedMiB > r.SizeMiB
			if over && prevOver {
				s.violations = append(s.violations, fmt.Sprintf("%s: %d MiB reserved, size %d (and above it 100 ms earlier)",
					time.Now().Format(time.StampMicro), r.ReservedMiB, r.SizeMiB))
			}
			prevOver, prev = over, r.ReservedMiB
			s.mu.Unlock()
		}
	}()
	var once sync.Once
	s.stop = func() { once.Do(func() { close(quit); <-finished }) }
	return s
}

// check stops the sampler and fails the test on a persisting violation, on any Snapshot error,
// or when it took fewer than minSamples samples.
func (s *reservedSampler) check(t *testing.T, minSamples int) {
	t.Helper()
	s.stop()
	if len(s.errs) > 0 {
		t.Errorf("the sampler's Snapshot failed %d times; first: %v", len(s.errs), s.errs[0])
	}
	if s.samples < minSamples {
		t.Errorf("the sampler took %d samples, want at least %d", s.samples, minSamples)
	}
	if len(s.violations) > 0 {
		t.Errorf("the reservations exceeded the size: %v", s.violations)
	}
	t.Logf("the sampler took %d samples; the most reserved that persisted: %d MiB", s.samples, s.maxReserved)
}

// ---------------------------------------------------------------- wall-clock steps

// A wall-clock step of every process changes nothing, because freshness is observed on each
// observer's monotonic clock and never computed from an mtime. The head A needs both units
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
			e.WriteConfig(memConfig(2))
			x := e.Run("X", "--mem", "128M")
			e.WaitStarted("X")

			a := e.Pausing("admit.before-enter", "ticket.refreshed").Run("A", "--mem", "256M")
			defer a.AutoRelease("admit.before-enter")()
			autoRefresh := a.AutoRelease("ticket.refreshed")
			defer func() { autoRefresh() }()
			tkA := e.WaitTicket("A")
			b := e.Run("B", "--mem", "128M")
			e.Eventually(startWait, "B is queued", func() bool {
				tk, queued := e.Ticket("B")
				_, held := e.Hold("B")
				return queued && !tk.Dead || held
			})
			noHold(t, e, "B", "B passed the live head A")
			waitRefreshed(e, "A", tkA.MtimeNS)

			// catchRefreshed holds A just after a refresh; it returns that refresh's mtime.
			catchRefreshed := func() int64 {
				t.Helper()
				autoRefresh()
				a.WaitReached("ticket.refreshed")
				tk, ok := e.Ticket("A")
				if !ok || tk.Dead {
					t.Fatal("A, held after a refresh, has no live ticket")
				}
				return tk.MtimeNS
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
// observable, a unit that the head cannot use but a later waiter could is not given to the
// later waiter (no backfill behind a live head), and the head is admitted first when its
// minimum frees up.
//
// Then the order holds from the resumption itself, before any refresh is observable. The head
// W3 needs two units and W4, behind it, needs one; both stop and one unit frees up meanwhile.
// W3 stops first and W4 2 s later, so W4 last saw W3's final mtime before its own stop. W4 is
// continued first and W3 about 3 s later: W4 has watched W3 unchanged for at most about 7 s (W3's
// last refresh may precede its stop by up to 2 s, W4 watches 2 s more before its own stop, and
// 3 s after its continuation), leaving about 3 s below the 10 s of a stall, since the 15 s in
// which W4 itself made no pass were not watched (PROTOCOL.md section 10.2). So W3 is not STALLED
// for it and W4 must not take the unit. Once the rest frees up, W3 is admitted
// before W4.
//
// Last, with capacity freed while every waiter is stopped, both remaining waiters (each fits)
// are admitted within the I5 bound of the resumption; which goes first is not asserted, as they
// are continued together.
//
// Mutation: a STALLED verdict sticks (Observer.Observe ignores a changed mtime once the ticket
// is stalled): after the resumption W2 keeps passing over the live head W1 and takes the unit.
// Mutation: count the observer's own absence as watched (Observer.Pass never adds a gap above
// MaxPassGap to the unwatched time): W4 finds W3 STALLED as soon as it is continued and takes
// the free unit in the second phase.
// Mutation: a resumed waiter waits too long before its next election (refresh, on a refresh gap
// above 10 s, also defers the next election by 25 s): in the third phase the waiter that was
// not caught mid-attempt is admitted only after the bound.
//
// -short skips the third phase.
func TestSuspendAllWaiters(t *testing.T) {
	e := State(t)
	e.WriteConfig(memConfig(3))
	x1 := e.Run("X1", "--mem", "128M")
	e.WaitHold("X1", startWait)
	x2 := e.Run("X2", "--mem", "256M")
	e.WaitHold("X2", startWait)

	w1, autoW1 := startStoppableHead(e, "W1", "--mem", "256M")
	e.WaitTicket("W1")
	w2 := e.Run("W2", "--mem", "128M")
	e.WaitTicket("W2")
	w3, autoW3 := startStoppableHead(e, "W3", "--mem", "256M")
	e.WaitTicket("W3")
	w4 := e.Run("W4", "--mem", "128M")
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
		before[name] = tk.MtimeNS
	}
	time.Sleep(time.Until(stoppedAt.Add(suspendFor)))
	for _, p := range []*Proc{w4, w3, w2, w1} {
		p.Cont()
	}
	defer w1.AutoRelease("admit.before-enter")()
	autoW3 = w3.AutoRelease("admit.before-enter")
	defer func() { autoW3() }()

	// Phase 1: no capacity was freed. Once every refresh can have been observed, one unit frees
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
	noHold(t, e, "W3", "W3 was admitted with no unit free")

	// Phase 2: the head W3 needs two units, W4 behind it one. W3 stops first, and W4 once its
	// elections (one per second) can have observed W3's last refresh, so that W3's mtime does not
	// change between W4's last pass before the suspension and its first pass after it. Meanwhile
	// W2 finishes, freeing one unit. W4 is continued first and W3 resumptionGap later.
	const resumptionGap = 3 * time.Second
	stoppedAt, mtimeW3 := stopOutsideSection(e, w3, autoW3)
	autoW3 = w3.AutoRelease("admit.before-enter")
	time.Sleep(2 * time.Second)
	e.SectionLeft()
	w4.Stop()
	sectionFreeOf(t, e, w4.PID())
	if tk, ok := e.Ticket("W3"); !ok || tk.MtimeNS != mtimeW3 {
		t.Fatalf("the stopped W3's ticket changed or went away while W4 watched it (%v)", ok)
	}
	e.ReleaseChild("W2")
	w2.ExitCode(0)
	time.Sleep(time.Until(stoppedAt.Add(suspendFor)))
	w4.Cont()
	continuedW4 := time.Now()
	for time.Since(continuedW4) < resumptionGap {
		noHold(t, e, "W4", "W4 took the free unit from the stopped head W3 right after its own resumption")
		time.Sleep(pollStep)
	}
	w3.Cont()
	t.Logf("W3 continued %v after W4", time.Since(continuedW4))
	waitRefreshed(e, "W3", mtimeW3)
	time.Sleep(observeWait)
	noHold(t, e, "W4", "W4 was admitted past the live head W3, which needs two units with one free")
	noHold(t, e, "W3", "W3 was admitted with one unit free")
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

	// Phase 3: W3 and W4 hold every unit. W5 is the head and W6 waits behind it; both stop and
	// meanwhile W3 and W4 finish, freeing every unit.
	w5, autoW5 := startStoppableHead(e, "W5", "--mem", "128M")
	e.WaitTicket("W5")
	w6 := e.Run("W6", "--mem", "128M")
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
	var first engine.HolderInfo
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
	for _, h := range []engine.HolderInfo{h5, h6} {
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

// A shrink from 14 to 12 units while holds reserve 4 (I3): the reservations are counted whatever
// the size, so the reduced budget is 12 minus those 4, and no admission brings the reservations
// above 12. Releasing a hold frees capacity again (partial release); growing back to 14 lets a
// request use the 2 units more.
//
// Mutation: count in reservedMiB only the holds published after the last size change: R1 and
// R2 are both admitted and 13 units are reserved under a size of 12.
func TestShrinkWithHoldsAboveTheSize(t *testing.T) {
	e := State(t)
	e.WriteConfig(memConfig(14))
	l := e.Run("L", "--mem", "1280M")
	e.WaitStarted("L")
	h1 := e.Run("H1", "--mem", "256M")
	e.WaitStarted("H1")
	h2 := e.Run("H2", "--mem", "256M")
	e.WaitStarted("H2")
	e.ReleaseChild("L")
	l.ExitCode(0)

	writeConfigAtomic(e, memConfig(12)+"# shrunk\n")
	if r := e.Status().Mem; r.SizeMiB != 12*128 || stableReserved(e) != 4 {
		t.Fatalf("after the shrink: size %d MiB, %d units reserved; want 12 units and 4", r.SizeMiB, stableReserved(e))
	}

	// A request during the reduced period takes from what is free.
	w, err := e.Engine().EnqueueContext(context.Background(), spec.Request{Label: "held-a", NeedMiB: 3 * 128})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), startWait)
	defer cancel()
	ga, err := w.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ga.Close()
	if ga.GrantMiB() != 3*128 {
		t.Fatalf("request: grant %d MiB, want 3 units", ga.GrantMiB())
	}

	// A queued request may take at most what is left below the size: 5 units.
	r1 := e.Run("R1", "--mem", "640M")
	e.WaitStarted("R1")
	if got := stableReserved(e); got != 12 {
		t.Errorf("%d units reserved under a size of 12", got)
	}

	// Nothing is free: a request waits.
	r2 := e.Run("R2", "--mem", "128M")
	e.WaitTicket("R2")
	time.Sleep(observeWait)
	noHold(t, e, "R2", "R2 was admitted with 12 units reserved under a size of 12")
	if got := stableReserved(e); got != 12 {
		t.Errorf("%d units reserved under a size of 12", got)
	}

	// Partial release: H2 goes, and R2 fits.
	e.ReleaseChild("H2")
	h2.ExitCode(0)
	e.WaitStarted("R2")

	// Growth back to 14: 3 units are free.
	writeConfigAtomic(e, memConfig(14))
	r3 := e.Run("R3", "--mem", "384M")
	e.WaitStarted("R3")
	if got := stableReserved(e); got != 14 {
		t.Errorf("%d units reserved, want 14", got)
	}
	for _, p := range []*Proc{h1, r1, r2, r3} {
		e.ReleaseChild(p.name)
		p.ExitCode(0)
	}
}

// ---------------------------------------------------------------- unknown format

// layout reads every file of the state directory, the run log included: relative path ->
// content.
func layout(t *testing.T, e Env) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(e.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(e.Dir, path)
		if d.IsDir() {
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
// wait. The waiters find it as they next enter the section and fail open: their commands run
// unbudgeted, holding no hold. A run that arrives afterwards does the same without queueing.
// The reservations never exceed the size, and nobody writes anything: no ticket, hold, gen bump,
// admit.lock body, degraded record or run-log line. The format-1 holder's release, whose section
// finds FORMAT 2, leaves its hold in place rather than removing it under a format it does not
// implement, and the holder writes no run-log line either: it re-reads FORMAT just before the
// append (PROTOCOL.md section 20). The run log never comes to exist.
//
// Mutation: skip the re-read of FORMAT before the append (appendRunLog appends after the
// rotation check alone): H's line appears.
func TestUnknownFormatRunsUnbudgeted(t *testing.T) {
	e := State(t)
	e.WriteConfig(memConfig(3))
	h := e.Run("H", "--mem", "256M")
	hh := e.WaitHold("H", startWait)
	w1 := e.Run("W1", "--mem", "256M")
	e.WaitTicket("W1")
	w2 := e.Run("W2", "--mem", "128M")
	e.WaitTicket("W2")
	sampler := sampleReserved(e)
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

	n := e.Run("N", "--mem", "128M")
	for name, want := range map[string]string{"W1": "256", "W2": "128", "N": "128"} {
		r := e.WaitStarted(name)
		if r.Env["CORESLOT_UNBUDGETED"] == "" || r.Env["CORESLOT_GRANTED_MEM"] != want || r.Env["CORESLOT_HELD_FD"] != "" {
			t.Errorf("%s's command environment %v", name, r.Env)
		}
		if _, ok := r.FDs[3]; ok {
			t.Errorf("%s's command has fd 3 open: %v", name, r.FDs)
		}
	}
	if _, ok := e.Ticket("N"); ok {
		t.Error("N published a ticket under FORMAT 2")
	}
	if got := stableReserved(e); got != 2 {
		t.Errorf("%d units reserved, want 2 (H's)", got)
	}
	for _, name := range []string{"H", "W1", "W2", "N"} {
		e.ReleaseChild(name)
	}
	for _, p := range []*Proc{h, w1, w2, n} {
		p.ExitCode(0)
	}
	sampler.check(t, 5)
	for _, p := range []*Proc{w1, w2, n} {
		if !strings.Contains(p.Stderr(), "format 2") {
			t.Errorf("%s did not name format 2: %q", p.name, p.Stderr())
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
		e.WriteConfig(memConfig(1))
		x := e.Run("X", "--mem", "128M")
		e.WaitHold("X", startWait)
		w := e.Pausing("admit.before-enter").Run("W", "--mem", "128M")
		autoW := w.AutoRelease("admit.before-enter")
		tk := e.WaitTicket("W")
		// W is caught after an election, before a section; X then frees the unit, so that
		// section would admit it.
		autoW()
		w.WaitReached("admit.before-enter")
		e.ReleaseChild("X")
		x.ExitCode(0)
		if got := e.Status().Mem.FreeMiB; got != 128 {
			t.Fatalf("%d MiB free, want 128: W's attempt would not admit it", got)
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
		if len(rep.Holds) != 0 || len(rep.DeadHolds) != 0 || len(rep.Tickets) != 0 || len(rep.Cancels) != 0 ||
			rep.Mem.ReservedMiB != 0 {
			t.Errorf("after the cancellation: holds %v, dead %v, tickets %v, cancels %v, reserved %d MiB",
				rep.Holds, rep.DeadHolds, rep.Tickets, rep.Cancels, rep.Mem.ReservedMiB)
		}
	})

	t.Run("after", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(memConfig(1))
		v := e.Pausing("publish.after-create").Run("V", "--mem", "128M")
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
		if rep := e.Status(); len(rep.Cancels) != 0 {
			t.Errorf("a cancel request was left for the admitted ticket: %v", rep.Cancels)
		}
		e.ReleaseChild("V")
		v.ExitCode(0)
	})

	t.Run("after, past the bound", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(memConfig(1))
		v := e.Pausing("publish.after-create").Run("V", "--mem", "128M")
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
		if rep := e.Status(); len(rep.Cancels) != 0 {
			t.Errorf("a refused cancel published a request: %v", rep.Cancels)
		}
		v.Release("publish.after-create")
		defer v.AutoRelease("publish.after-create")()
		e.WaitStarted("V")
		e.WaitHold("V", startWait)
		e.ReleaseChild("V")
		v.ExitCode(0)
	})
}
