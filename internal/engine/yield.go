//go:build unix

package engine

import (
	"context"
	"time"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// The yield of a long head to short jobs (PROTOCOL.md section 10.3).
const (
	yieldLongS  = 600             // a head yields only when its typical run time is at least this
	yieldShortS = 120             // a waiter is short when its typical run time is at most this
	yieldShare  = 4               // the yield lasts the head's typical run time over this
	yieldGrace  = 3 * time.Second // a yield nobody takes ends after the head fits this long
	yieldMargin = 60 * time.Second
)

// yieldState is the head's own record of its yield, in its process.
type yieldState struct {
	active, done bool          // a yield runs now; the ticket has yielded (it never yields again)
	start, span  time.Duration // when it started and how long it may last, on the monotonic clock
	fitSince     time.Duration // since when the head has fit in a row, on the monotonic clock; -1 when it does not
	lastPass     int64         // the record's last_pass_ns when fitSince was last reset
	untilNS      int64
	ended        string // why the yield ended, "" while it runs or before
}

// short reports whether a waiter of need, typical run time typicalS and restart is short beside a
// head of needH: its typical run time is known and at most yieldShortS, it is no restart, it fits
// room and would not fit beside the head.
func short(need, typicalS int64, restart bool, needH, room int64) bool {
	return !restart && typicalS > 0 && typicalS <= yieldShortS && need <= room && need > room-needH
}

// overrunLive reports whether a live hold that arrived at or after headOrder, a passer, has run
// longer than its known typical run time. A hold with none never counts.
func overrunLive(holds []liveHold, headOrder state.Key, nowNS int64) bool {
	for _, h := range holds {
		b := h.body
		if !h.unparsed && uint64(b.ArrivalNS) >= headOrder.OrderKey && b.TypicalS > 0 && nowNS-b.StartNS > b.TypicalS*int64(time.Second) {
			return true
		}
	}
	return false
}

// yieldEnds ends a running yield, inside sec, when its record is missing or another's, its
// deadline or its span on the monotonic clock has passed, or a passer overruns; otherwise it keeps
// the record's last pass.
func (t *Ticket) yieldEnds(sec *store.Section, holds []liveHold) error {
	rec, ok, err := t.e.st.ReadYield()
	if err != nil {
		return err
	}
	now, why := t.e.clock.Realtime(), ""
	switch {
	case !ok || rec.Head != t.id.String():
		t.yield.active, t.yield.ended = false, "record" // another may have removed it as stale: keep its file
		return nil
	case now >= rec.UntilNS:
		why = "deadline"
	case t.e.clock.Monotonic()-t.yield.start >= t.yield.span:
		why = "clock"
	case overrunLive(holds, t.order, now):
		why = "overrun"
	default:
		if rec.LastPassNS != t.yield.lastPass {
			t.yield.lastPass, t.yield.fitSince = rec.LastPassNS, -1
		}
		return nil
	}
	return t.endYield(sec, why)
}

// endYield ends the running yield for why and removes its record.
func (t *Ticket) endYield(sec *store.Section, why string) error {
	t.yield.active, t.yield.ended = false, why
	return sec.RemoveYield()
}

// yieldWait decides, for a head that would be admitted now, whether it waits instead: while a
// running yield has a taker, or when it starts one. It starts one when it fits a known room, the
// kernel's pressure is normal, its typical run time is at least yieldLongS, it is no restart or
// container request, it was never moved or yielded before, its own deadline leaves room for it,
// and a short waiter it would displace is behind it. The decision comes before the level gate
// records its start, so a yield uses no spacing slot.
func (t *Ticket) yieldWait(ctx context.Context, sec *store.Section, room int64, m config.Machine) (bool, error) {
	y, now, mono := &t.yield, t.e.clock.Realtime(), t.e.clock.Monotonic()
	if y.active {
		if y.fitSince < 0 {
			y.fitSince = mono
		}
		if mono-y.fitSince < yieldGrace {
			t.out.Reason, t.out.YieldUntilNS = ReasonYielding, y.untilNS
			return true, nil
		}
		return false, t.endYield(sec, "no_taker")
	}
	if err := sec.RemoveYield(); err != nil { // the head now: any record left is stale
		return false, err
	}
	need := t.req.NeedMiB
	if y.done || t.req.Restart || t.req.RealMemory || t.req.TypicalS < yieldLongS || !m.HeadroomOK || need > room {
		return false, nil
	}
	if level, ok := KernelPressure(); !ok || level != PressureNormal {
		return false, nil
	}
	until := now + t.req.TypicalS*int64(time.Second)/yieldShare
	if d, ok := ctx.Deadline(); ok {
		until = min(until, now+int64(time.Until(d)-yieldMargin))
	}
	if until <= now+int64(yieldGrace) {
		return false, nil
	}
	if found, err := t.shortBehind(room); err != nil || !found {
		return false, err
	}
	rec := store.Yield{Head: t.id.String(), StartNS: now, UntilNS: until}
	if err := sec.SetYield(rec); err != nil {
		return false, err
	}
	*y = yieldState{active: true, done: true, start: mono, span: time.Duration(until - now), fitSince: mono, untilNS: until}
	t.out.Reason, t.out.YieldUntilNS = ReasonYielding, until
	return true, nil
}

// shortBehind reports whether a short waiter is behind this head, which was never moved: a live
// ticket after it in effective order, without a note, short beside it in room.
func (t *Ticket) shortBehind(room int64) (bool, error) {
	entries, err := t.e.st.ListNames(store.QueueDir)
	if err != nil {
		return false, err
	}
	orders, _, err := t.e.orders()
	if err != nil {
		return false, err
	}
	if _, moved := orders[t.id]; moved {
		return false, nil
	}
	notes, err := t.e.notes()
	if err != nil {
		return false, err
	}
	for _, en := range entries {
		if en.ID == t.id || !t.order.Less(orderKey(en.ID, orders)) || notes[en.Name] {
			continue
		}
		tk, err := t.ticketBody(en.Name)
		if err != nil {
			return false, err
		}
		if tk == nil || !short(tk.NeedMiB, tk.TypicalS, tk.Restart, t.req.NeedMiB, room) {
			continue
		}
		dead, err := probe(t.e.st, store.QueueDir, en.Name)
		if err == nil && !dead {
			return true, nil
		}
		if err != nil && !gone(err) && !stray(err) {
			return false, err
		}
	}
	return false, nil
}

// shortPass is the first condition that keeps this ticket from passing head as short, "" when
// none does: a valid record names head, this ticket is short beside it, expected to end by the
// record's deadline, no passer overruns, and the kernel's pressure is normal. ok false means no
// valid record names head, and today's passes decide.
func (t *Ticket) shortPass(head wire.Ticket, headKey, headOrder state.Key, holds []liveHold, room int64) (why string, ok bool) {
	rec, valid, err := t.e.st.ReadYield()
	now := t.e.clock.Realtime()
	if err != nil || !valid || rec.Head != wire.ID(headKey).String() || now >= rec.UntilNS {
		return "", false
	}
	level, read := KernelPressure()
	switch {
	case t.req.Restart || t.req.TypicalS == 0 || t.req.TypicalS > yieldShortS || t.noted:
		return "not_short", true
	case !short(t.req.NeedMiB, t.req.TypicalS, false, head.NeedMiB, room):
		return "not_displaced", true
	case now+t.req.TypicalS*int64(time.Second) > rec.UntilNS:
		return "ends_late", true
	case overrunLive(holds, headOrder, now):
		return "overrun_live", true
	case !read || level != PressureNormal:
		return "pressure_not_normal", true
	}
	return "", true
}

// notePass sets the record's last_pass_ns inside sec after a short pass admitted this ticket.
func (t *Ticket) notePass(sec *store.Section) {
	if rec, ok, err := t.e.st.ReadYield(); err == nil && ok {
		rec.LastPassNS = t.e.clock.Realtime()
		_ = sec.SetYield(rec) // a lost write only lets the head end its yield sooner, by no taker
	}
}
