//go:build unix

package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// A ticket says its run's typical run time, so that a head can tell the short waiters behind it.
// Mutation: the enqueue leaving typical_s out reads 0.
func TestTicketCarriesTypicalS(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	tk := enqueue(t, e, spec.Request{Label: "short", NeedMiB: 64, TypicalS: 90})
	body, err := os.ReadFile(filepath.Join(dir, store.QueueDir, tk.Name()))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := wire.DecodeTicket(body); err != nil || b.TypicalS != 90 {
		t.Errorf("ticket typical_s %d (%v), want 90", b.TypicalS, err)
	}
}

// headReq is a head of 1024 MiB with a typical run time of 40 min: its yield lasts 10 min.
var headReq = spec.Request{Label: "h", NeedMiB: 1024, TypicalS: 2400}

// shortReq is a short waiter of need MiB with a typical run time of typicalS.
func shortReq(need, typicalS int64) spec.Request {
	return spec.Request{Label: "s", NeedMiB: need, TypicalS: typicalS}
}

// yieldScene is a room of 1536 MiB (with a 128 MiB hold live before it when holdLive), the head
// headReq, which fits it, and waiters reqs behind it.
func yieldScene(t *testing.T, holdLive bool, reqs ...spec.Request) (*Engine, *fakeClock, *Ticket, []*Ticket) {
	t.Helper()
	room := int64(1536)
	if holdLive {
		room += 128
	}
	clk := newFakeClock()
	e := newEngine(t, capped(t, room), clk)
	if holdLive {
		hold(t, e, need(128))
	}
	h := enqueue(t, e, headReq)
	var ws []*Ticket
	for _, r := range reqs {
		ws = append(ws, enqueue(t, e, r))
	}
	return e, clk, h, ws
}

// record is the yield record, ok false when there is none.
func record(t *testing.T, e *Engine) (store.Yield, bool) {
	t.Helper()
	y, ok, err := e.st.ReadYield()
	if err != nil {
		t.Fatal(err)
	}
	return y, ok
}

// wantYielding asserts that a step of h leaves it waiting, yielding until untilNS when it is not 0.
func wantYielding(t *testing.T, h *Ticket, untilNS int64) {
	t.Helper()
	w := waitOutcome(t, h)
	if w.Outcome.Reason != ReasonYielding || untilNS != 0 && w.Outcome.YieldUntilNS != untilNS {
		t.Fatalf("head: %+v; want it yielding until %d", w.Outcome, untilNS)
	}
}

// wantYieldEnded asserts that a step of h admits it, its yield ended for why.
func wantYieldEnded(t *testing.T, h *Ticket, why string) {
	t.Helper()
	if g := admitted(t, step(t, h)); g.Admission().YieldEnded != why {
		t.Errorf("head admitted with its yield ended for %q, want %q", g.Admission().YieldEnded, why)
	}
}

// A long head that fits yields to a short waiter it would displace, with a hold live and with
// none: it publishes a deadline of a quarter of its typical run time, and the waiter passes it as
// short and notes its pass. A waiter that fits beside it does not make it yield. Mutation: dropping
// the displacement test makes the head yield to the waiter that fits beside it.
func TestHeadYieldsToAShortWaiterItWouldDisplace(t *testing.T) {
	for _, holdLive := range []bool{true, false} {
		e, clk, h, ws := yieldScene(t, holdLive, shortReq(768, 60))
		now := clk.clock().Realtime()
		wantYielding(t, h, now+600e9)
		if y, ok := record(t, e); !ok || y.Head != ticketID(h).String() || y.StartNS != now {
			t.Fatalf("hold live %v: record %+v, %v", holdLive, y, ok)
		}
		g := admitted(t, step(t, ws[0]))
		if g.Admission().Pass != "short" {
			t.Errorf("hold live %v: the waiter passed by %q, want short", holdLive, g.Admission().Pass)
		}
		if y, _ := record(t, e); y.LastPassNS != now {
			t.Errorf("hold live %v: last_pass_ns %d, want %d", holdLive, y.LastPassNS, now)
		}
	}
	_, _, h, _ := yieldScene(t, false, shortReq(256, 60)) // fits beside the head: 256 <= 1536 - 1024
	if g := admitted(t, step(t, h)); g.Admission().YieldEnded != "" {
		t.Errorf("the head yielded: %+v", g.Admission())
	}
}

// The deadline ends the yield, on the wall clock, though the monotonic clock has not moved; the
// record goes and the head starts. Mutation: no deadline leaves the head yielding.
func TestYieldEndsAtTheDeadline(t *testing.T) {
	e, clk, h, _ := yieldScene(t, false, shortReq(768, 60))
	wantYielding(t, h, 0)
	clk.mu.Lock()
	clk.real += 601e9
	clk.mono += HeadMaxBackoff // the head's next attempt, well within no taker
	clk.mu.Unlock()
	wantYieldEnded(t, h, "deadline")
	if _, ok := record(t, e); ok {
		t.Error("the record outlived the yield")
	}
}

// The head's own monotonic clock ends the yield after a backward step of the wall clock. Mutation:
// the wall clock alone keeps it until no taker ends it.
func TestYieldEndsByTheMonotonicClock(t *testing.T) {
	_, clk, h, _ := yieldScene(t, false, shortReq(768, 60))
	wantYielding(t, h, 0)
	clk.mu.Lock()
	clk.real -= int64(time.Hour)
	clk.mono += 601 * time.Second
	clk.mu.Unlock()
	wantYieldEnded(t, h, "clock")
}

// A yield nobody takes ends once the head has fit for 3 s with no pass. Mutation: dropping no taker
// leaves the head yielding.
func TestYieldNobodyTakesEnds(t *testing.T) {
	_, clk, h, _ := yieldScene(t, true, shortReq(768, 60))
	wantYielding(t, h, 0)
	clk.advance(2500 * time.Millisecond)
	wantYielding(t, h, 0)
	clk.advance(HeadMaxBackoff)
	wantYieldEnded(t, h, "no_taker")
}

// A pass between two polls of the head, which never saw the passer, restarts the head's count, as
// the record's last_pass_ns says. Mutation: a passer that does not note its pass lets the head end
// the yield by no taker.
func TestAPassRestartsTheNoTakerCount(t *testing.T) {
	_, clk, h, ws := yieldScene(t, false, shortReq(768, 60))
	wantYielding(t, h, 0)
	clk.advance(2500 * time.Millisecond)
	admitted(t, step(t, ws[0])).Close()
	clk.advance(time.Second)
	wantYielding(t, h, 0)
}

// A record that is gone (another head removed it as stale) ends the yield, latched: the head starts
// and never yields again. Mutation: a head that keeps yielding without its record waits on.
func TestYieldEndsWhenItsRecordIsGone(t *testing.T) {
	e, clk, h, _ := yieldScene(t, false, shortReq(768, 60))
	wantYielding(t, h, 0)
	sec, err := e.st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = sec.RemoveYield()
	sec.Leave()
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(HeadMaxBackoff)
	wantYieldEnded(t, h, "record")
}

// A passer past its typical run time ends the yield and stops a second pass; a hold with no
// typical run time never counts as an overrun. Mutations: dropping the overrun check keeps the
// yield and lets the second waiter pass; counting a hold with typical_s 0 ends the yield at once.
func TestAnOverrunEndsTheYield(t *testing.T) {
	e, clk, h, ws := yieldScene(t, false, shortReq(768, 60), shortReq(768, 30))
	// A hold that arrived after the head, long running, with no typical run time and no need.
	now := clk.clock().Realtime()
	lockHere(t, writeHold(t, e.st.Dir(), wire.Hold{Label: "untimed", ArrivalNS: now + 1, StartNS: now - int64(time.Hour)}))
	wantYielding(t, h, 0)
	admitted(t, step(t, ws[0]))
	clk.advance(61 * time.Second)
	w := waitOutcome(t, ws[1])
	if w.Outcome.NoPass != "overrun_live" {
		t.Errorf("the second waiter: %+v; want no_pass overrun_live", w.Outcome)
	}
	if w := waitOutcome(t, h); w.Outcome.YieldEnded != "overrun" {
		t.Errorf("the head: %+v; want its yield ended by the overrun", w.Outcome)
	}
	if _, ok := record(t, e); ok {
		t.Error("the record outlived the overrun")
	}
}

// A restart, a moved head, an unread headroom, a pressure that is not normal and a typical run time
// under 10 min never yield: each head starts at once.
func TestHeadsThatNeverYield(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, e *Engine, h *Ticket)
		req   spec.Request
	}{
		{"restart", nil, spec.Request{Label: "h", NeedMiB: 1024, TypicalS: 2400, Restart: true}},
		{"short head", nil, spec.Request{Label: "h", NeedMiB: 1024, TypicalS: 599}},
		{"moved", func(t *testing.T, e *Engine, h *Ticket) {
			sec, err := e.st.Enter(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer sec.Leave()
			if err := sec.PublishOrder(ticketID(h), []byte(`{"order_ns":1}`)); err != nil {
				t.Fatal(err)
			}
		}, headReq},
		{"pressure", func(t *testing.T, _ *Engine, _ *Ticket) { setPressure(t, 2, true) }, headReq},
		{"headroom unread", func(t *testing.T, _ *Engine, _ *Ticket) { testHeadroom.ok = false }, headReq},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk := newFakeClock()
			e := newEngine(t, capped(t, 1536), clk)
			h := enqueue(t, e, c.req)
			enqueue(t, e, shortReq(768, 60))
			if c.setup != nil {
				c.setup(t, e, h)
			}
			if g := admitted(t, step(t, h)); g.Admission().YieldEnded != "" {
				t.Errorf("%+v", g.Admission())
			}
			if _, ok := record(t, e); ok {
				t.Error("a record was written")
			}
		})
	}
}

// A head with a wait deadline caps its yield at that deadline less 60 s, and does not yield when
// that leaves 3 s or less. Mutation: dropping the cap yields 10 min, past the deadline.
func TestYieldIsCappedByTheHeadsDeadline(t *testing.T) {
	_, clk, h, _ := yieldScene(t, false, shortReq(768, 60))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Second)
	defer cancel()
	r, err := h.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := clk.clock().Realtime() + 140e9
	if w, ok := r.(Waiting); !ok || w.Outcome.Reason != ReasonYielding || w.Outcome.YieldUntilNS > want || w.Outcome.YieldUntilNS < want-2e9 {
		t.Fatalf("head under a 200 s deadline: %#v; want it yielding until about %d", r, want)
	}

	_, _, h, _ = yieldScene(t, false, shortReq(768, 60))
	ctx, cancel = context.WithTimeout(context.Background(), 62*time.Second)
	defer cancel()
	if r, err := h.Step(ctx); err != nil || !isAdmitted(r) {
		t.Errorf("head in its last minute: %#v, %v; want it admitted at once", r, err)
	}
	if a, ok := r.(Admitted); ok {
		a.Close()
	}
}

// A yield records no spacing slot, where an admission would. Mutation: deciding the yield after the
// gate records its start moves last_admit_ns.
func TestYieldRecordsNoSpacingSlot(t *testing.T) {
	e, clk, h, _ := yieldScene(t, true, shortReq(768, 60))
	setWatch(t, e, clk, watch.Detector{YellowClearedNS: sampleWallNS - 1e9}, 0)
	wantYielding(t, h, 0)
	if d, _, _, _ := e.st.ReadWatch(); d.LastAdmitNS != 0 {
		t.Errorf("last_admit_ns %d after a yield, want 0", d.LastAdmitNS)
	}
}

// A waiter passes as short only while a valid record names the head and only when expected to end
// by its deadline: not once the record is gone, nor past its deadline. Two short waiters start side
// by side when both fit. Mutations: judging the waiter without the record passes it; dropping the
// deadline test passes a waiter that would end late.
func TestShortPassNeedsAValidRecord(t *testing.T) {
	_, clk, h, ws := yieldScene(t, false, shortReq(768, 120), shortReq(768, 60), shortReq(768, 60))
	wantYielding(t, h, 0)
	clk.mu.Lock()
	clk.real += 500e9 // 100 s before the deadline: a 120 s job would end late
	clk.mu.Unlock()
	if w := waitOutcome(t, ws[0]); w.Outcome.NoPass != "ends_late" {
		t.Errorf("a waiter that would end late: %+v; want ends_late", w.Outcome)
	}
	admitted(t, step(t, ws[1]))
	admitted(t, step(t, ws[2])) // side by side: 768 + 768 fit 1536

	for _, end := range []string{"removed", "deadline"} {
		e, clk, h, ws := yieldScene(t, false, shortReq(768, 60))
		wantYielding(t, h, 0)
		if end == "removed" {
			sec, err := e.st.Enter(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = sec.RemoveYield()
			sec.Leave()
			if err != nil {
				t.Fatal(err)
			}
		} else {
			clk.mu.Lock()
			clk.real += 601e9
			clk.mu.Unlock()
		}
		if isAdmitted(step(t, ws[0])) {
			t.Errorf("a waiter passed with the record %s", end)
		}
	}
}

// A second short waiter passes after the first ends, within 3 s of the head fitting again: the count
// restarts on the pass and when the head stops fitting. Mutation: a count that restarts on neither
// ends the yield by no taker the moment the first passer ends.
func TestASecondPassAfterTheFirstEnds(t *testing.T) {
	_, clk, h, ws := yieldScene(t, false, shortReq(768, 60), shortReq(1000, 60))
	wantYielding(t, h, 0)
	first := admitted(t, step(t, ws[0]))
	clk.advance(5 * time.Second)
	if w := waitOutcome(t, h); w.Outcome.Reason != ReasonRoomShort {
		t.Fatalf("head beside the passer: %+v", w.Outcome)
	}
	first.Close()
	clk.advance(HeadMaxBackoff)
	wantYielding(t, h, 0)
	clk.advance(time.Second)
	if g := admitted(t, step(t, ws[1])); g.Admission().Pass != "short" {
		t.Errorf("the second waiter passed by %q, want short", g.Admission().Pass)
	}
}

// The count also restarts when something else than a passer takes the room: the head that fits
// again has 3 s more. Mutation: a count that restarts only on a pass ends the yield by no taker as
// soon as the head fits again.
func TestNoTakerCountsFromWhenTheHeadFitsAgain(t *testing.T) {
	e, clk, h, _ := yieldScene(t, false, shortReq(768, 60))
	wantYielding(t, h, 0)
	f, err := store.OpenFile(holdPath(t, e.st.Dir(), 1024), os.O_RDONLY) // another run, ahead of the head
	if err != nil {
		t.Fatal(err)
	}
	if err := lockNB(f); err != nil {
		t.Fatal(err)
	}
	clk.advance(5 * time.Second)
	if w := waitOutcome(t, h); w.Outcome.Reason != ReasonRoomShort {
		t.Fatalf("head beside the other run: %+v", w.Outcome)
	}
	f.Close()
	clk.advance(HeadMaxBackoff)
	wantYielding(t, h, 0)
}

// A record that names another head, left by an earlier head, lets nobody pass. Mutation: a waiter
// that does not compare the record's head passes.
func TestARecordOfAnotherHeadLetsNobodyPass(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, capped(t, 1536), clk)
	hold(t, e, need(128))
	enqueue(t, e, spec.Request{Label: "h", NeedMiB: 1536}) // it cannot fit, and never yields
	w := enqueue(t, e, shortReq(768, 60))
	sec, err := e.st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other := wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String()
	err = sec.SetYield(store.Yield{Head: other, StartNS: 1, UntilNS: clk.clock().Realtime() + int64(time.Hour)})
	sec.Leave()
	if err != nil {
		t.Fatal(err)
	}
	if isAdmitted(step(t, w)) {
		t.Error("a waiter passed on another head's record")
	}
}

// A dead ticket behind the head is no short waiter: the head starts at once. Mutation: a short
// waiter judged without its lock probe makes the head yield to it.
func TestADeadTicketIsNoShortWaiter(t *testing.T) {
	clk := newFakeClock()
	dir := capped(t, 1536)
	e := newEngine(t, dir, clk)
	h := enqueue(t, e, headReq)
	id := wire.ID{OrderKey: uint64(clk.clock().Realtime() + 1e9), PID: 4999, Seq: 1}
	writeTicket(t, dir, id, wire.Ticket{Label: "s", NeedMiB: 768, CreatedNS: 1, TypicalS: 60}) // nobody locks it
	if g := admitted(t, step(t, h)); g.Admission().YieldEnded != "" {
		t.Errorf("%+v", g.Admission())
	}
	if _, ok := record(t, e); ok {
		t.Error("the head yielded to a dead ticket")
	}
}
