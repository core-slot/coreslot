//go:build unix

package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

func move(t *testing.T, e *Engine, tk *Ticket, place Place, ref *Ticket) int64 {
	t.Helper()
	var r wire.ID
	if ref != nil {
		r = ticketID(ref)
	}
	n, err := e.MoveContext(t.Context(), ticketID(tk), place, r)
	if err != nil {
		t.Fatalf("MoveContext: %v", err)
	}
	return n
}

// queueOrder is the queue as status lists it: the labels in effective order.
func queueOrder(t *testing.T, e *Engine) string {
	t.Helper()
	rep, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	s := ""
	for _, tk := range rep.Tickets {
		s += tk.Label
	}
	return s
}

// A ticket moved first is the head for every election: here neither fits, and C, moved ahead of B,
// waits for room as the head instead of behind B. Mutation: the election's rescan ignoring order/
// keeps C at position 2.
func TestMoveFirstMakesTheHead(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, capped(t, 2048), clk)
	hold(t, e, need(1024))
	b := enqueue(t, e, needLabel(2048, "b"))
	clk.advance(time.Second)
	c := enqueue(t, e, needLabel(2048, "c"))
	wantReason(t, waitOutcome(t, c), ReasonBehindTicket, "")
	move(t, e, c, PlaceFirst, nil)
	clk.advance(time.Second) // the next election, which the move's gen bump makes rescan
	w := waitOutcome(t, c)
	if w.Position != 1 || w.Outcome.Reason != ReasonRoomShort {
		t.Errorf("C after --first: position %d, reason %q; want the head waiting for room", w.Position, w.Outcome.Reason)
	}
	if p := waitOutcome(t, b).Position; p != 2 {
		t.Errorf("B's position %d after C moved first, want 2", p)
	}
}

// A ticket moved first that fits starts: the re-election in the section reads order/ too, and its
// override goes with its ticket. Mutations: the re-election ignoring order/ keeps C out; leaving the
// override behind at admission keeps order/ non-empty.
func TestMoveFirstAdmitsAndRemovesTheOverride(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, capped(t, 2048), clk)
	hold(t, e, need(1024))
	enqueue(t, e, needLabel(2048, "b"))
	clk.advance(time.Second)
	c := enqueue(t, e, needLabel(512, "c"))
	wantReason(t, waitOutcome(t, c), ReasonBehindTicket, "")
	move(t, e, c, PlaceFirst, nil)
	clk.advance(time.Second)
	admitted(t, step(t, c))
	if names, _ := os.ReadDir(filepath.Join(e.st.Dir(), store.OrderDir)); len(names) != 0 {
		t.Errorf("order/ holds %v after C was admitted", names)
	}
}

// The keys a move writes put the ticket where it says, and status lists the queue in that order;
// a ticket or reference that is not waiting is refused with nothing written. Mutations: status
// listing by name reddens the order; --before writing the reference's own key ties it, and pid
// and seq then put C behind B.
func TestMovePlaces(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, capped(t, 2048), clk)
	hold(t, e, need(1024))
	a := enqueue(t, e, needLabel(2048, "a"))
	clk.advance(time.Second)
	b := enqueue(t, e, needLabel(2048, "b"))
	clk.advance(time.Second)
	c := enqueue(t, e, needLabel(2048, "c"))
	for _, step := range []struct {
		tk    *Ticket
		place Place
		ref   *Ticket
		want  string
	}{
		{c, PlaceBefore, b, "acb"},
		{a, PlaceLast, nil, "cba"},
		{a, PlaceFirst, nil, "acb"},
		{a, PlaceAfter, b, "cba"},
		{c, PlaceAfter, a, "bac"},
	} {
		move(t, e, step.tk, step.place, step.ref)
		if got := queueOrder(t, e); got != step.want {
			t.Errorf("after moving %s: queue %q, want %q", step.tk.req.Label, got, step.want)
		}
	}
	gone := wire.ID{OrderKey: 1, PID: 1, Seq: 1}
	if _, err := e.MoveContext(t.Context(), gone, PlaceFirst, wire.ID{}); !errors.Is(err, ErrNoTicket) {
		t.Errorf("moving a ticket that is not waiting: %v, want ErrNoTicket", err)
	}
	if _, err := e.MoveContext(t.Context(), ticketID(a), PlaceBefore, gone); !errors.Is(err, ErrNoTicket) {
		t.Errorf("moving before a ticket that is not waiting: %v, want ErrNoTicket", err)
	}
	if got := queueOrder(t, e); got != "bac" {
		t.Errorf("a refused move changed the queue to %q", got)
	}
}

// The passing rule's "arrived after the head" compares with the head's effective key: a head moved
// to the front is protected as one that arrived first. D arrives, then C, which is moved first; D
// goes out of turn past C, and E then waits, because D arrived after C's new key, though before
// C's name. Mutation: comparing with the head's name key lets E go out of turn beside D.
func TestMovedHeadKeepsOutOfTurnOneAtATime(t *testing.T) {
	e, clk, _, _ := unknownHeadScene(t)
	clk.advance(time.Second)
	d := enqueue(t, e, spec.Request{Label: "d", NeedMiB: 256, TypicalS: 2})
	x := enqueue(t, e, spec.Request{Label: "e", NeedMiB: 256, TypicalS: 2})
	clk.advance(time.Second)
	c := enqueue(t, e, spec.Request{Label: "c", NeedMiB: 2048})
	move(t, e, c, PlaceFirst, nil)
	if watchSteps(t, clk, d, 6) == 0 {
		t.Fatal("D never went out of turn: the test shows nothing")
	}
	for i := 0; i < 4; i++ {
		wantReason(t, waitOutcome(t, x), ReasonBehindHead, "out_of_turn_live")
		clk.advance(time.Second)
	}
}
