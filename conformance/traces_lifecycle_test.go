//go:build coreslot_testhooks && unix

package conformance

import (
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

	"github.com/Nezhinskiy/coreslot"
	"github.com/Nezhinskiy/coreslot/internal/store"
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

// indices are the cpu token indices the live hold labelled label lists.
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

// lockedSampler records the largest number of locked cpu tokens seen, and the size then, until
// stop is called; it reads through the library's Snapshot every 20 ms.
type lockedSampler struct {
	stop      func()
	mu        sync.Mutex
	maxLocked int
	overSize  []string
}

func sampleLocked(e Env) *lockedSampler {
	s := &lockedSampler{}
	quit, finished := make(chan struct{}), make(chan struct{})
	c := e.Client()
	go func() {
		defer close(finished)
		for {
			select {
			case <-quit:
				return
			case <-time.After(pollStep):
			}
			rep, err := c.Snapshot()
			if err != nil {
				continue
			}
			r := rep.Resources["cpu"]
			s.mu.Lock()
			s.maxLocked = max(s.maxLocked, r.Locked)
			if r.Locked > r.Size {
				s.overSize = append(s.overSize, fmt.Sprintf("%s: %d locked, size %d", time.Now().Format(time.StampMicro), r.Locked, r.Size))
			}
			s.mu.Unlock()
		}
	}()
	var once sync.Once
	s.stop = func() { once.Do(func() { close(quit); <-finished }) }
	return s
}

// ---------------------------------------------------------------- wall-clock steps

// A wall-clock step between the head's last refresh and its observer changes nothing:
// freshness is observed on the observer's monotonic clock, never computed from an mtime. B's
// clock is an hour behind (backward) or ahead (forward) of the head A's, so every mtime A
// writes, its last one included, reads to B as an hour off. While A refreshes, B waits behind
// it for longer than the stall threshold; once A stops, B is admitted within the bound.
//
// Mutation: judge STALLED by comparing the mtime with the observer's wall clock (wall clock
// minus mtime above 10 s) instead of observing changes. Forward reddens at once (B passes the
// live head); backward reddens at the bound (A's mtime lies in B's future, so A never stalls).
//
// -short runs the backward step only.
func TestClockStepThenHeadStops(t *testing.T) {
	for _, c := range []struct {
		name string
		step time.Duration
		prio string // keeps B behind A: its arrival is stepped too
	}{
		{"backward", -time.Hour, "-61"},
		{"forward", time.Hour, "0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if testing.Short() && c.step > 0 {
				t.Skip("-short: the forward step is not run")
			}
			e := State(t)
			e.WriteConfig(cpuConfig(2))
			x := e.Run("X", "--need", "cpu=1")
			e.WaitHold("X", startWait)

			// A needs both tokens and waits at the head; B needs the free one but waits behind it.
			a, autoA := startStoppableHead(e, "A", "--need", "cpu=2")
			tkA := e.WaitTicket("A")
			b := e.Offset(c.step).Run("B", "--priority", c.prio, "--need", "cpu=1")
			e.Eventually(startWait, "B is queued", func() bool {
				tk, queued := e.Ticket("B")
				_, held := e.Hold("B")
				return queued && !tk.Dead || held
			})
			noHold(t, e, "B", "B passed the live head A at once: its stepped clock made A's refreshes look stale")
			if rep := e.Status(); len(rep.Tickets) != 2 || rep.Tickets[0].Label != "A" {
				t.Fatalf("queue %v: A is not ahead of B", rep.Tickets)
			}

			// A refreshes; B, an hour off, must keep seeing it live past the stall threshold.
			waitRefreshed(e, "A", tkA.MtimeNS)
			time.Sleep(12 * time.Second)
			noHold(t, e, "B", "B passed the live head A: its stepped clock made A's refreshes look stale")

			stoppedAt, _ := stopOutsideSection(e, a, autoA)
			hb := e.WaitHold("B", stalledHeadBound+startWait)
			at := admittedAt(hb).Add(-c.step) // B's start_ns is on its own, stepped, clock
			if at.Before(stoppedAt) {
				t.Fatalf("B was admitted at %v, before A stopped at %v", at, stoppedAt)
			}
			if d := at.Sub(stoppedAt); d > stalledHeadBound {
				t.Fatalf("B was admitted %v after the head stopped, bound %v (load: %s)", d, stalledHeadBound, uptime())
			} else {
				t.Logf("B was admitted %v after the head stopped", d)
			}

			a.Cont()
			defer a.AutoRelease("admit.before-enter")()
			e.ReleaseChild("B")
			b.ExitCode(0)
			e.ReleaseChild("X")
			x.ExitCode(0)
			e.WaitHold("A", startWait)
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
// minimum frees up. Then, with capacity freed while every waiter is stopped, someone is
// admitted within the I5 bound of the resumption.
//
// Which waiter is admitted first in the second phase is not asserted: right after a common
// resumption each waiter has observed no change of the others for 15 s, so until it observes
// their refreshes they are STALLED for it, and I4 lets it pass over them.
//
// Mutation: a STALLED verdict sticks (Observer.Observe ignores a changed mtime once the ticket
// is stalled): after the resumption W2 keeps passing over the live head W1 and takes the token.
//
// -short skips the second phase.
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
	w3, autoW3 := startStoppableHead(e, "W3", "--need", "cpu=1")
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
		before[name] = tk.MtimeNS
	}
	time.Sleep(time.Until(stoppedAt.Add(suspendFor)))
	for _, p := range []*Proc{w4, w3, w2, w1} {
		p.Cont()
	}
	defer w1.AutoRelease("admit.before-enter")()
	autoW3 = w3.AutoRelease("admit.before-enter")
	defer func() { autoW3() }()

	// Phase 1: no capacity was freed. Once every refresh can have been observed, one token frees
	// up: W2-W4 could use it, the head W1 cannot, and nobody may take it.
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

	if testing.Short() {
		t.Log("-short: the second phase (capacity freed during a common suspension) is not run")
		for _, name := range []string{"W1", "W2", "W3", "W4"} {
			e.ReleaseChild(name)
		}
		for _, p := range []*Proc{w1, w2, w3, w4} {
			p.ExitCode(0)
		}
		return
	}

	// Phase 2: W3 is the head and W4 waits behind it. Both stop; meanwhile W1 and W2 finish,
	// freeing every token.
	e.SectionLeft()
	w4.Stop()
	sectionFreeOf(t, e, w4.PID())
	stoppedAt, _ = stopOutsideSection(e, w3, autoW3)
	autoW3 = w3.AutoRelease("admit.before-enter")
	e.ReleaseChild("W1")
	e.ReleaseChild("W2")
	w1.ExitCode(0)
	w2.ExitCode(0)
	time.Sleep(time.Until(stoppedAt.Add(suspendFor)))
	w4.Cont()
	w3.Cont()
	resumedAt := time.Now()
	var first coreslot.HolderInfo
	e.Eventually(stalledHeadBound+startWait, "a waiter is admitted after the resumption", func() bool {
		for _, name := range []string{"W3", "W4"} {
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
	h3 := e.WaitHold("W3", startWait)
	h4 := e.WaitHold("W4", startWait)
	t.Logf("W3 admitted at %v, W4 at %v", admittedAt(h3), admittedAt(h4))
	e.ReleaseChild("W3")
	e.ReleaseChild("W4")
	w3.ExitCode(0)
	w4.ExitCode(0)
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
	e.WaitHold("L", startWait)
	h1 := e.Run("H1", "--need", "cpu=2")
	e.WaitHold("H1", startWait)
	h2 := e.Run("H2", "--need", "cpu=2")
	e.WaitHold("H2", startWait)
	if got := indices(e, "H1"); !slices.Equal(got, []int{10, 11}) {
		t.Fatalf("H1 holds %v, want 10-11", got)
	}
	if got := indices(e, "H2"); !slices.Equal(got, []int{12, 13}) {
		t.Fatalf("H2 holds %v, want 12-13", got)
	}
	e.ReleaseChild("L")
	l.ExitCode(0)

	writeConfigAtomic(e, cpuConfig(12)+"# shrunk\n")
	if r := e.Status().Resources["cpu"]; r.Size != 12 || cpuLocked(e) != 4 {
		t.Fatalf("after the shrink: size %d, %d tokens locked; want 12 and 4", r.Size, cpuLocked(e))
	}

	// Adoption during the reduced period takes from what is free.
	c := e.Client()
	ga, short, err := c.Adopt(coreslot.Request{Label: "adopt-a", Need: map[string]coreslot.Range{"cpu": {Min: 2, Max: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ga.Close()
	if ga.Grant()["cpu"] != 3 || short["cpu"] != 0 || !slices.Equal(indices(e, "adopt-a"), span(0, 2)) {
		t.Fatalf("adoption: grant %v, shortfall %v, indices %v; want 3 at 0-2", ga.Grant(), short, indices(e, "adopt-a"))
	}

	// A queued request may take at most what is left below the size: 5, from 3-7.
	r1 := e.Run("R1", "--need", "cpu=1..12")
	e.WaitHold("R1", startWait)
	if got := indices(e, "R1"); !slices.Equal(got, span(3, 7)) {
		t.Errorf("R1 holds %v, want 3-7", got)
	}
	if got := cpuLocked(e); got != 12 {
		t.Errorf("%d tokens locked under a size of 12", got)
	}

	// Nothing is free: an adoption takes nothing and reports its shortfall; a request waits.
	gb, short, err := c.Adopt(coreslot.Request{Label: "adopt-b", Need: map[string]coreslot.Range{"cpu": {Min: 1, Max: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	defer gb.Close()
	if gb.Grant()["cpu"] != 0 || short["cpu"] != 1 {
		t.Errorf("adoption with nothing free: grant %v, shortfall %v; want none and a shortfall of 1", gb.Grant(), short)
	}
	r2 := e.Run("R2", "--need", "cpu=1")
	e.WaitTicket("R2")
	time.Sleep(observeWait)
	noHold(t, e, "R2", "R2 was admitted with 12 tokens locked under a size of 12")
	if got := cpuLocked(e); got != 12 {
		t.Errorf("%d tokens locked under a size of 12", got)
	}

	// Partial release: the holds above the new size go, and R2 fits at index 8.
	e.ReleaseChild("H2")
	h2.ExitCode(0)
	e.WaitHold("R2", startWait)
	if got := indices(e, "R2"); !slices.Equal(got, []int{8}) {
		t.Errorf("R2 holds %v, want 8", got)
	}

	// Growth back to 14: 9, 12 and 13 are free (10 and 11 are H1's).
	writeConfigAtomic(e, cpuConfig(14))
	r3 := e.Run("R3", "--need", "cpu=3")
	e.WaitHold("R3", startWait)
	if got := indices(e, "R3"); !slices.Equal(got, []int{9, 12, 13}) {
		t.Errorf("R3 holds %v, want 9, 12 and 13", got)
	}
	if got := cpuLocked(e); got != 14 {
		t.Errorf("%d tokens locked, want 14", got)
	}
	for _, p := range []*Proc{h1, r1, r2, r3} {
		e.ReleaseChild(p.name)
		p.ExitCode(0)
	}
}

// ---------------------------------------------------------------- unknown format

// layout reads every file of the state directory except the token files: relative path ->
// content. The run log is read apart.
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
		if rel == filepath.Join(store.LogDir, store.RunLog) {
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
// rather than removing it under a format it does not implement.
//
// Mutation: a token-only run appends its run-log line (runChild given the store's log for a
// token-only grant): lines for W1, W2 and N appear.
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
	if got := cpuLocked(e); got != 3 {
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
	sampler.stop()
	if sampler.maxLocked > 3 || len(sampler.overSize) > 0 {
		t.Errorf("locked tokens exceeded the size: max %d, %v", sampler.maxLocked, sampler.overSize)
	}
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
		for _, l := range runLog(t, e) {
			if l["label"] != "H" {
				t.Errorf("a run-log line was written under FORMAT 2: %v", l)
			}
		}
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
// nothing is left. One published while the owner is inside the section that admits it waits
// for the section, finds no ticket, and has no effect: cancellation never revokes a grant.
//
// Mutations: skip admission step 1's cancel check (the "before" run is admitted and its command
// runs); publish a cancel request without checking that the ticket exists (the "after" cancel
// succeeds and leaves a request behind).
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
		if got := e.Status().Resources["cpu"].Free; got != 1 {
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
		if len(rep.Holds) != 0 || len(rep.DeadHolds) != 0 || len(rep.Tickets) != 0 || len(rep.Cancels) != 0 ||
			rep.Resources["cpu"].Locked != 0 {
			t.Errorf("after the cancellation: holds %v, dead %v, tickets %v, cancels %v, locked %d",
				rep.Holds, rep.DeadHolds, rep.Tickets, rep.Cancels, rep.Resources["cpu"].Locked)
		}
	})

	t.Run("after", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(cpuConfig(1))
		v := e.Pausing("publish.after-create").Run("V", "--need", "cpu=1")
		v.CatchPublication("publish.after-create", store.HoldsDir) // inside the admitting section
		tk, ok := e.Ticket("V")
		if !ok {
			t.Fatal("V has no ticket inside its admitting section")
		}
		c := e.Start("cancel", tk.ID)
		time.Sleep(time.Second) // the canceller waits for the section V holds
		if c.Exited() {
			t.Fatalf("cancel exited %d while V held the section; stderr:\n%s", c.Wait(), c.Stderr())
		}
		v.Release("publish.after-create")
		defer v.AutoRelease("publish.after-create")()
		c.ExitCode(exitRefusal)
		if !strings.Contains(c.Stderr(), "no waiting ticket") {
			t.Errorf("cancel stderr %q", c.Stderr())
		}
		e.WaitStarted("V")
		e.WaitHold("V", startWait)
		if rep := e.Status(); len(rep.Cancels) != 0 {
			t.Errorf("a cancel request was left for the admitted ticket: %v", rep.Cancels)
		}
		e.ReleaseChild("V")
		v.ExitCode(0)
	})
}
