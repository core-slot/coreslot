//go:build unix

package engine

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/store"
)

// waitWork counts a waiter's filesystem work through the seams of wait.go.
type waitWork struct {
	lstats, probes, holdProbes, genReads, cancelChecks int
}

// countWaitWork replaces the seams with counting wrappers for the rest of the test.
func countWaitWork(t *testing.T) *waitWork {
	t.Helper()
	w := &waitWork{}
	oldLstat, oldProbe, oldGen, oldCancel := lstat, probe, genRead, checkCancel
	lstat = func(p string) (os.FileInfo, error) {
		w.lstats++
		return oldLstat(p)
	}
	probe = func(st *store.Store, dir, name string) (bool, error) {
		switch dir {
		case store.QueueDir:
			w.probes++
		case store.HoldsDir:
			w.holdProbes++
		}
		return oldProbe(st, dir, name)
	}
	genRead = func(st *store.Store) (store.Gen, error) {
		w.genReads++
		return oldGen(st)
	}
	checkCancel = func(tk *Ticket) (bool, error) {
		w.cancelChecks++
		return oldCancel(tk)
	}
	t.Cleanup(func() { lstat, probe, genRead, checkCancel = oldLstat, oldProbe, oldGen, oldCancel })
	return w
}

// The election examines the tickets ahead from the nearest and stops at the first live,
// non-STALLED one (PROTOCOL.md section 10.3): a waiter with a live ticket directly ahead makes
// one lock probe per election wherever it is in the queue, and reads one mtime for it plus one
// for each ticket of the front window.
func TestElectionStopsAtFirstLiveTicket(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 7)) // nobody is admitted
	const k = 20
	var tks []*Ticket
	for i := 0; i < k; i++ {
		tks = append(tks, enqueue(t, e, need("cpu=1"))) // same order key: ascending seq
	}
	last := tks[k-1]
	work := countWaitWork(t)
	for pass := 0; pass < 5; pass++ {
		for _, tk := range tks[:k-1] {
			step(t, tk) // the tickets ahead stay fresh
		}
		*work = waitWork{}
		if p := position(t, step(t, last)); p != k {
			t.Fatalf("pass %d: position %d with %d live tickets ahead, want %d", pass, p, k-1, k)
		}
		if work.lstats != 1+FrontWindow || work.probes != 1 {
			t.Fatalf("pass %d: %d mtime reads and %d probes behind %d live tickets, want %d and 1",
				pass, work.lstats, work.probes, k-1, 1+FrontWindow)
		}
		clk.advance(time.Second)
	}
}

// Two consecutive stopped tickets at the front of the queue: both are in the front window, so
// the waiter behind them watches both from the start and passes both within the I5 bound.
func TestConsecutiveStoppedTickets(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "enqueue", dir, "cpu=1") // the head, stopped
	startHelper(t, "enqueue", dir, "cpu=1") // the next, stopped
	s := enqueue(t, e, need("cpu=1"))
	if p := position(t, step(t, s)); p != 3 {
		t.Fatalf("position %d behind two fresh tickets, want 3", p)
	}
	for sec := 1; sec <= 10; sec++ {
		clk.advance(time.Second)
		r := step(t, s)
		if sec < 10 {
			if p := position(t, r); p != 3 {
				t.Fatalf("%d s: position %d, want 3", sec, p)
			}
		} else if !isAdmitted(r) {
			t.Fatalf("%d s: want admitted past both stopped tickets, got %#v", sec, r)
		}
	}
}

// A stopped head with three waiters behind it: every waiter watches the head through the front
// window, so all three are admitted in the same second, 10 s after it stopped, not 10 s apart
// as each one's turn comes to be nearest to it (I5).
func TestStoppedHeadPassedByEverySuccessor(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "enqueue", dir, "cpu=1") // the head, stopped
	ws := []*Ticket{enqueue(t, e, need("cpu=1")), enqueue(t, e, need("cpu=1")), enqueue(t, e, need("cpu=1"))}
	admitted := map[int]int{} // waiter -> second
	for sec := 0; sec <= 11 && len(admitted) < len(ws); sec++ {
		for i, w := range ws {
			if _, done := admitted[i]; !done && isAdmitted(step(t, w)) {
				admitted[i] = sec
			}
		}
		clk.advance(time.Second)
	}
	for i := range ws {
		if sec, ok := admitted[i]; !ok || sec != 10 {
			t.Fatalf("admitted at %v (waiter -> second), want all three at 10 s", admitted)
		}
	}
}

// A ticket the election stopped short of, outside the front window, is not watched, and the
// unwatched time does not count towards its stall: the 4 s watched before the gap count, the
// 10 s gap does not.
func TestUnwatchedIntervalIsNotAStall(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	var front []*helper
	for i := 0; i < FrontWindow; i++ { // the front window, kept fresh
		front = append(front, startHelper(t, "enqueue", dir, "cpu=1"))
	}
	startHelper(t, "enqueue", dir, "cpu=1")      // x: stopped, beyond the front window
	y := startHelper(t, "enqueue", dir, "cpu=1") // y: between x and s, refreshed on command
	s := enqueue(t, e, need("cpu=1"))
	at := func(sec, want int) {
		t.Helper()
		for _, f := range front {
			f.command(t, "refresh")
		}
		if p := position(t, step(t, s)); p != want {
			t.Fatalf("%d s: position %d, want %d", sec, p, want)
		}
	}
	all := FrontWindow + 3 // the front window, x and y ahead
	at(0, all)
	for sec := 1; sec <= 14; sec++ { // y stalls at 10 s; x is watched from 10 s to 14 s
		clk.advance(time.Second)
		if sec < 10 {
			at(sec, all)
		} else {
			at(sec, all-1)
		}
	}
	y.command(t, "refresh")
	for sec := 15; sec <= 30; sec++ { // y is live from 15 s to 24 s: x unwatched, then 25..30 s
		clk.advance(time.Second)
		if sec < 25 {
			at(sec, all)
		} else {
			at(sec, all-1) // y passed over, x watched for 4+sec-25 s
		}
	}
	clk.advance(time.Second) // 31 s: x watched for 10 s and passed over; the front window ends the scan
	at(31, all-2)
}

// drive Steps tk whenever its next work is due, as Wait does, for d of the injected clock,
// calling before ahead of each Step, and returns the times of the Steps.
func drive(t *testing.T, clk *fakeClock, tk *Ticket, d time.Duration, before func()) []time.Duration {
	t.Helper()
	start := clk.clock().Monotonic()
	var at []time.Duration
	for clk.clock().Monotonic()-start < d {
		at = append(at, clk.clock().Monotonic()-start)
		before()
		if isAdmitted(step(t, tk)) {
			t.Fatal("admitted")
		}
		due := tk.Due()
		if due <= 0 {
			t.Fatalf("Due %v right after a Step", due)
		}
		clk.advance(due)
	}
	return at
}

// A waiter that is not the head, Stepped when its work is due (as Wait does), does all of its
// work once per second: one Step, one refresh, one cancel check, one gen read and one election
// examining one ticket. It never probes the holds: they are listed only on request.
func TestNonHeadWorksOncePerSecond(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	if !isAdmitted(step(t, enqueue(t, e, need("cpu=1")))) { // a live hold
		t.Fatal("not admitted on a free budget")
	}
	ahead := enqueue(t, e, need("cpu=1")) // never stepped; kept fresh below
	w := enqueue(t, e, need("cpu=1"))
	refreshes := 0
	defaultPoint := point
	point = func(name string) {
		if name == "ticket.refreshed" {
			refreshes++
		}
	}
	t.Cleanup(func() { point = defaultPoint })
	work := countWaitWork(t)
	const secs = 30
	at := drive(t, clk, w, secs*time.Second, func() {
		if err := ahead.refresh(clk.clock().Monotonic()); err != nil {
			t.Fatal(err)
		}
	})
	refreshes -= len(at) // ahead's
	// The first Step comes right after the enqueue, which refreshed: it does not refresh again.
	if len(at) != secs || refreshes != secs-1 || work.cancelChecks != secs || work.genReads != secs ||
		work.probes != secs || work.lstats != secs || work.holdProbes != 0 {
		t.Fatalf("in %d s: %d Steps, %d refreshes, %d cancel checks, %d gen reads, %d mtime reads, %d probes, %d hold probes; want %d of each (refreshes one fewer, hold probes 0)",
			secs, len(at), refreshes, work.cancelChecks, work.genReads, work.lstats, work.probes, work.holdProbes, secs)
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i] - at[i-1]; gap != ElectEvery {
			t.Fatalf("Steps %v apart (Steps at %v), want %v", gap, at, ElectEvery)
		}
	}
}

// The head, Stepped when its work is due, still retries admission on its backoff: 0.25 s after
// the first attempt, then 0.5 s, 1 s and 2 s.
func TestHeadRetriesOnBackoff(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 7)) // the head cannot be admitted
	tk := enqueue(t, e, need("cpu=1"))
	at := drive(t, clk, tk, 2*time.Second, func() {})
	if len(at) < 2 || at[1] != HeadFirstBackoff {
		t.Fatalf("Steps at %v, want the second at %v", at, HeadFirstBackoff)
	}
	if tk.attempts != 4 { // at 0, 0.25, 0.75 and 1.75 s
		t.Fatalf("%d admission attempts in 2 s (Steps at %v), want 4", tk.attempts, at)
	}
}

// Stepped when Due says, a head whose section entries all wait out their deadline still
// refreshes at most RefreshEvery/2 apart (see RefreshEvery), although its backoff reaches 2 s.
func TestRefreshCadenceWhenDue(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	tk := enqueue(t, e, need("cpu=1"))
	oldEnter := enterSection
	enterSection = func(st *store.Store, ctx context.Context) (*store.Section, error) {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("a Step's section entry has no deadline")
			return nil, context.DeadlineExceeded
		}
		clk.advance(time.Until(dl))
		return nil, context.DeadlineExceeded
	}
	defaultPoint := point
	var refreshes []time.Duration
	point = func(name string) {
		if name == "ticket.refreshed" {
			refreshes = append(refreshes, clk.clock().Monotonic())
		}
	}
	t.Cleanup(func() { enterSection, point = oldEnter, defaultPoint })

	start := clk.clock().Monotonic()
	for clk.clock().Monotonic()-start < 60*time.Second {
		if _, err := tk.Step(context.Background()); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		clk.advance(tk.Due())
	}
	const slack = 5 * time.Millisecond // time.Until(dl) is a hair under sectionWait
	prev := start
	for _, r := range append(refreshes, clk.clock().Monotonic()) {
		if gap := r - prev; gap > RefreshEvery/2+slack {
			t.Fatalf("refresh gap %v above %v (refreshes at %v)", gap, RefreshEvery/2, refreshes)
		}
		prev = r
	}
	if tk.backoff != HeadMaxBackoff {
		t.Fatalf("backoff %v after 60 s of failed entries, want %v", tk.backoff, HeadMaxBackoff)
	}
}

// Holds are listed and probed only when asked for (Ticket.Holders, the CLI's waiting line), not
// on the elections that follow a change of gen.
func TestHoldersOnlyOnRequest(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	if !isAdmitted(step(t, enqueue(t, e, need("cpu=1")))) { // a live hold
		t.Fatal("not admitted on a free budget")
	}
	ahead := enqueue(t, e, need("cpu=1"))
	w := enqueue(t, e, need("cpu=1"))
	work := countWaitWork(t)
	for i := 0; i < 5; i++ {
		before := readGen(t, e)
		if err := enqueue(t, e, need("cpu=1")).Withdraw(); err != nil { // gen changes
			t.Fatal(err)
		}
		if readGen(t, e) == before {
			t.Fatal("gen did not change")
		}
		if err := ahead.refresh(clk.clock().Monotonic()); err != nil {
			t.Fatal(err)
		}
		step(t, w)
		clk.advance(time.Second)
	}
	if work.holdProbes != 0 {
		t.Fatalf("%d hold probes over 5 elections after gen changes, want 0", work.holdProbes)
	}
	hs, err := w.Holders()
	if err != nil || len(hs) != 1 || work.holdProbes != 1 {
		t.Fatalf("Holders: %v, %v after %d probes; want the one live hold, one probe", hs, err, work.holdProbes)
	}
}
