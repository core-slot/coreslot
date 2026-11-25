//go:build unix

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// markID marks id in dir from another client, as `coreslot quiet` or `coreslot no-stop` does.
func markID(t *testing.T, dir, kind string, id wire.ID) {
	t.Helper()
	if _, err := newEngine(t, dir, nil).MarkContext(context.Background(), kind, id); err != nil {
		t.Fatalf("MarkContext(%s, %s): %v", kind, id, err)
	}
}

func unmarkID(t *testing.T, dir, kind string, id wire.ID) {
	t.Helper()
	if n, err := newEngine(t, dir, nil).UnmarkContext(context.Background(), kind, id); err != nil || n == 0 {
		t.Fatalf("UnmarkContext(%s, %s) = %d, %v", kind, id, n, err)
	}
}

func holdIDOf(g *Grant) wire.ID {
	id, _ := wire.ParseName(g.HoldName())
	return id
}

func isMarked(dir, kind string, id wire.ID) bool {
	return exists(filepath.Join(dir, kind, id.String()))
}

// A quiet live hold holds back every admission path: the head that fits, a waiter that would go out
// of turn past a head whose wait is unknown, and a container request; each waits with the reason
// quiet, and once the mark goes the head starts. Mutations: applying step 1c only to the head lets C
// go out of turn; skipping container requests admits U.
func TestQuietBlocksEveryPath(t *testing.T) {
	e, clk, a, b := unknownHeadScene(t)
	dir := e.st.Dir()
	markID(t, dir, store.QuietDir, holdIDOf(a))
	c := enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 5})
	if got := watchSteps(t, clk, c, 10); got != 0 {
		t.Errorf("C went out of turn at step %d past a quiet hold", got)
	}
	wantReason(t, waitOutcome(t, c), ReasonQuiet, "")
	wantReason(t, waitOutcome(t, b), ReasonQuiet, "")
	u := enqueue(t, e, spec.Request{Label: "u", NeedMiB: 256, RealMemory: true})
	wantReason(t, waitOutcome(t, u), ReasonQuiet, "")
	unmarkID(t, dir, store.QuietDir, holdIDOf(a))
	clk.advance(time.Second)
	if !isAdmitted(step(t, u)) {
		t.Error("the container request still waits once the mark is gone")
	}
}

// A quiet mark on a waiting ticket holds nothing back and moves nothing: a waiter goes out of turn
// past the marked head as it would past an unmarked one, and the head keeps its place. Mutation:
// blocking on any quiet mark keeps C waiting.
func TestQuietWaitingTicketBlocksNothing(t *testing.T) {
	e, clk, _, b := unknownHeadScene(t)
	markID(t, e.st.Dir(), store.QuietDir, ticketID(b))
	c := enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 5})
	if got := watchSteps(t, clk, c, 10); got == 0 {
		t.Error("C stayed behind a quiet head that only waits")
	}
	if p := position(t, step(t, b)); p != 1 {
		t.Errorf("the marked head's position %d, want 1", p)
	}
}

// The ticket's marks move to its hold at its admission and at its forced start, in the section that
// publishes the hold: the next waiter is held back while it runs. Mutations: skipping the move at
// admission, or at a forced start, admits the waiter.
func TestMarksMoveToTheHold(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "admission", true: "forced"}[forced], func(t *testing.T) {
			dir := capped(t, 1024)
			clk := newFakeClock()
			e := newEngine(t, dir, clk)
			var g *Grant
			if forced {
				hold(t, e, need(1024))
				tk := enqueue(t, e, need(512))
				markID(t, dir, store.QuietDir, ticketID(tk))
				markID(t, dir, store.NoStopDir, ticketID(tk))
				requestStart(t, dir, clk, tk)
				g = admitted(t, step(t, tk))
				for _, k := range []string{store.QuietDir, store.NoStopDir} {
					if !isMarked(dir, k, holdIDOf(g)) || isMarked(dir, k, ticketID(tk)) {
						t.Errorf("%s: hold marked %v, ticket marked %v; want the mark moved", k, isMarked(dir, k, holdIDOf(g)), isMarked(dir, k, ticketID(tk)))
					}
				}
				return // the waiter test needs room; the admission case shows it
			}
			tk := enqueue(t, e, need(256))
			markID(t, dir, store.QuietDir, ticketID(tk))
			markID(t, dir, store.NoStopDir, ticketID(tk))
			g = admitted(t, step(t, tk))
			for _, k := range []string{store.QuietDir, store.NoStopDir} {
				if !isMarked(dir, k, holdIDOf(g)) || isMarked(dir, k, ticketID(tk)) {
					t.Errorf("%s: hold marked %v, ticket marked %v; want the mark moved", k, isMarked(dir, k, holdIDOf(g)), isMarked(dir, k, ticketID(tk)))
				}
			}
			w := enqueue(t, e, need(256))
			wantReason(t, waitOutcome(t, w), ReasonQuiet, "")
		})
	}
}

// A cancel and a withdrawal remove the ticket's marks with it. Mutation: leaving the mark directories
// out of RemoveOwnTicket keeps them, orphans until reconciliation.
func TestCancelAndWithdrawRemoveMarks(t *testing.T) {
	dir := capped(t, 128)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	hold(t, e, need(128))
	cancelled, withdrawn := enqueue(t, e, need(512)), enqueue(t, e, need(512))
	for _, tk := range []*Ticket{cancelled, withdrawn} {
		markID(t, dir, store.QuietDir, ticketID(tk))
		markID(t, dir, store.NoStopDir, ticketID(tk))
	}
	if err := newEngine(t, dir, nil).CancelContext(context.Background(), ticketID(cancelled)); err != nil {
		t.Fatal(err)
	}
	clk.advance(10 * time.Millisecond)
	if _, err := cancelled.Step(context.Background()); !errors.Is(err, ErrCanceled) {
		t.Fatalf("the cancelled ticket's step: %v", err)
	}
	if err := withdrawn.Withdraw(); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []*Ticket{cancelled, withdrawn} {
		for _, k := range []string{store.QuietDir, store.NoStopDir} {
			if isMarked(dir, k, ticketID(tk)) {
				t.Errorf("%s mark of %s outlived its ticket", k, tk.Name())
			}
		}
	}
}

// A quiet job whose run is SIGKILLed ends its window at once: its hold's lock goes, so the next
// attempt admits, long before reconciliation would remove the mark. Mutation: testing the hold's file
// instead of its lock keeps the waiter out.
func TestQuietEndsOnCrash(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	holder := startHelper(t, "hold", dir, "64")
	id, _ := wire.ParseName(holder.detail)
	markID(t, dir, store.QuietDir, id)
	tk := enqueue(t, e, need(64))
	wantReason(t, waitOutcome(t, tk), ReasonQuiet, "")
	holder.kill(t)
	clk.advance(time.Second)
	if !isAdmitted(step(t, tk)) {
		t.Error("the queue stays held after the quiet job's run was killed")
	}
}

// The release of an attempt's hold removes its marks, even when a descendant keeps the hold live (a
// stopped attempt whose command left the process group, before a restart, --no-wait, a third stop or
// a spent --timeout): the queue moves on. Mutation: removing the marks only when the hold itself goes
// keeps the window open.
func TestReleaseEndsTheWindow(t *testing.T) {
	dir := capped(t, 1024)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	g := hold(t, e, need(256))
	markID(t, dir, store.QuietDir, holdIDOf(g))
	markID(t, dir, store.NoStopDir, holdIDOf(g))
	fd, err := syscall.Dup(int(g.Files()[0].Fd())) // the descendant's copy of fd 3
	if err != nil {
		t.Fatal(err)
	}
	descendant := os.NewFile(uintptr(fd), "hold")
	defer descendant.Close()
	g.Close()
	if dead, err := e.st.Probe(store.HoldsDir, g.HoldName()); err != nil || dead {
		t.Fatalf("the hold: dead %v, %v; want kept live by the descendant", dead, err)
	}
	for _, k := range []string{store.QuietDir, store.NoStopDir} {
		if isMarked(dir, k, holdIDOf(g)) {
			t.Errorf("the released hold is still marked %s", k)
		}
	}
	if !isAdmitted(step(t, enqueue(t, e, need(256)))) {
		t.Error("a waiter is held back by a released attempt's descendant")
	}
}

// Two waiting quiet jobs start one after the other: the first admitted holds the second back. Two
// running quiet jobs both run, and the queue waits until the second ends. Mutation: exempting a
// quiet ticket from step 1c admits the second beside the first.
func TestSeveralQuietJobs(t *testing.T) {
	dir := capped(t, 2048)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	q1, q2 := enqueue(t, e, need(256)), enqueue(t, e, need(256))
	markID(t, dir, store.QuietDir, ticketID(q1))
	markID(t, dir, store.QuietDir, ticketID(q2))
	g1 := admitted(t, step(t, q1))
	clk.advance(time.Second)
	wantReason(t, waitOutcome(t, q2), ReasonQuiet, "")
	g1.Close()
	clk.advance(time.Second)
	admitted(t, step(t, q2)).Close()

	g3, g4 := hold(t, e, need(256)), hold(t, e, need(256))
	markID(t, dir, store.QuietDir, holdIDOf(g3))
	markID(t, dir, store.QuietDir, holdIDOf(g4))
	w := enqueue(t, e, need(256))
	wantReason(t, waitOutcome(t, w), ReasonQuiet, "")
	g3.Close()
	clk.advance(time.Second)
	wantReason(t, waitOutcome(t, w), ReasonQuiet, "")
	g4.Close()
	clk.advance(time.Second)
	if !isAdmitted(step(t, w)) {
		t.Error("the queue stays held after the last quiet job ended")
	}
}

// A forced start is admitted while a quiet hold runs: the user's later choice for one job. Mutation:
// testing the quiet marks before the start check keeps it waiting.
func TestStartNowPassesAQuietHold(t *testing.T) {
	dir := capped(t, 2048)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	g := hold(t, e, need(256))
	markID(t, dir, store.QuietDir, holdIDOf(g))
	tk := enqueue(t, e, need(256))
	wantReason(t, waitOutcome(t, tk), ReasonQuiet, "")
	requestStart(t, dir, clk, tk)
	if p := admitted(t, step(t, tk)).Admission().Pass; p != "forced" {
		t.Errorf("pass %q, want forced", p)
	}
}

// A snapshot carries the marks of the holds and the tickets. Mutation: reading them for the holds
// only leaves the ticket unmarked.
func TestSnapshotCarriesMarks(t *testing.T) {
	dir := capped(t, 256)
	e := newEngine(t, dir, newFakeClock())
	g := hold(t, e, need(256))
	tk := enqueue(t, e, need(256))
	markID(t, dir, store.QuietDir, holdIDOf(g))
	markID(t, dir, store.NoStopDir, ticketID(tk))
	r, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if h := r.Holds[0]; !h.Quiet || h.NoStop {
		t.Errorf("hold quiet %v, no-stop %v; want true, false", h.Quiet, h.NoStop)
	}
	if k := r.Tickets[0]; k.Quiet || !k.NoStop {
		t.Errorf("ticket quiet %v, no-stop %v; want false, true", k.Quiet, k.NoStop)
	}
}

// publishFakeHold publishes a live hold of pid, started at startNS, and returns its id and its
// descriptor, which keeps it live until the test ends or the caller closes it.
func publishFakeHold(t *testing.T, e *Engine, pid int, startNS int64, label string) (wire.ID, *os.File) {
	t.Helper()
	sec, err := e.st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	f, name, err := sec.PublishHold(wire.Hold{Label: label, PID: pid, StartNS: startNS, NeedMiB: 1}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	id, _ := wire.ParseName(name)
	return id, f
}

// An --independent run nests under a quiet job only when a live quiet-marked hold is an ancestor's:
// every such hold is tested, the newer of two included; a quiet hold of another process tree, and a
// mark naming a ticket or a dead hold, nest nothing. Mutations: testing only the first live mark
// misses the newer job; skipping isAncestor nests past an unrelated window; counting any mark nests
// under a ticket's or a dead hold's.
func TestQuietAncestor(t *testing.T) {
	dir := capped(t, 1024)
	e := newEngine(t, dir, newFakeClock())
	older, _ := publishFakeHold(t, e, 1001, 1791720000000000000, "older")
	newer, _ := publishFakeHold(t, e, 1002, 1791720001000000000, "newer")
	markID(t, dir, store.QuietDir, older)
	markID(t, dir, store.QuietDir, newer)
	if label, ok := QuietAncestor(dir, func(pid int) bool { return pid == 1002 }); !ok || label != "newer" {
		t.Errorf("under the newer quiet job: %q, %v; want newer, true", label, ok)
	}
	if label, ok := QuietAncestor(dir, func(int) bool { return false }); ok {
		t.Errorf("no quiet job is an ancestor, yet it nests under %q", label)
	}

	other := capped(t, 1024)
	e2 := newEngine(t, other, newFakeClock())
	tk := enqueue(t, e2, need(64))
	markID(t, other, store.QuietDir, ticketID(tk))
	dead, f := publishFakeHold(t, e2, 1003, 1791720002000000000, "dead")
	markID(t, other, store.QuietDir, dead)
	f.Close() // the hold dies with its mark in place
	publishFakeHold(t, e2, 1004, 1791720003000000000, "unmarked")
	if label, ok := QuietAncestor(other, func(int) bool { return true }); ok {
		t.Errorf("a mark on a ticket or a dead hold nests the run under %q", label)
	}
}

// A quiet window stops a yielding head's no-taker count: once the window ends, the yield, which
// still has time, waits 3 s more for a taker. Mutation: leaving the count running in step 1c ends
// the yield as no_taker at the head's first attempt after the window.
func TestQuietWindowRestartsTheNoTakerCount(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, capped(t, 1536+128), clk)
	g := hold(t, e, need(128))
	h := enqueue(t, e, headReq)
	enqueue(t, e, shortReq(768, 60))
	wantYielding(t, h, 0)
	dir := e.st.Dir()
	markID(t, dir, store.QuietDir, holdIDOf(g))
	clk.advance(5 * time.Second)
	wantReason(t, waitOutcome(t, h), ReasonQuiet, "")
	unmarkID(t, dir, store.QuietDir, holdIDOf(g))
	clk.advance(HeadMaxBackoff)
	wantYielding(t, h, 0)
}
