//go:build unix

package engine

import (
	"context"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
)

// waitOutcome runs one Step that must leave the ticket waiting.
func waitOutcome(t *testing.T, tk *Ticket) Waiting {
	t.Helper()
	w, ok := step(t, tk).(Waiting)
	if !ok {
		t.Fatal("admitted; the test needs a refusal")
	}
	return w
}

func wantReason(t *testing.T, w Waiting, reason WaitReason, noPass string) {
	t.Helper()
	if w.Outcome.Reason != reason || w.Outcome.NoPass != noPass {
		t.Errorf("reason %q, no_pass %q; want %q, %q (%+v)", w.Outcome.Reason, w.Outcome.NoPass, reason, noPass, w)
	}
}

// busySection makes every later section entry find the section busy.
func busySection(t *testing.T) {
	t.Helper()
	oldEnter, oldTry := enterSection, tryEnterSection
	enterSection = func(*store.Store, context.Context) (*store.Section, error) { return nil, context.DeadlineExceeded }
	tryEnterSection = func(*store.Store) (*store.Section, bool, error) { return nil, false, nil }
	t.Cleanup(func() { enterSection, tryEnterSection = oldEnter, oldTry })
}

func unknownHeadroom(t *testing.T) {
	t.Helper()
	saved := testHeadroom
	t.Cleanup(func() { testHeadroom = saved })
	testHeadroom.mib, testHeadroom.ok = 0, false
}

func typical(mib, seconds int64) spec.Request {
	return spec.Request{NeedMiB: mib, TypicalS: seconds}
}

func restart(mib int64) spec.Request {
	return spec.Request{NeedMiB: mib, Restart: true}
}

// watched is an engine with a 128 MiB hold live, so that the watch state gates admission, and
// that state stored as d, sampled just now.
func watched(t *testing.T, d watch.Detector) *Engine {
	t.Helper()
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	hold(t, e, need(128))
	setWatch(t, e, clk, d, 0)
	return e
}

// A head the room refuses says so, with the numbers it used. Mutation: the room branch sets
// behind_head, or the reading is written after the refusal.
func TestReasonRoomShort(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	hold(t, e, need(128))
	w := waitOutcome(t, enqueue(t, e, need(2048)))
	wantReason(t, w, ReasonRoomShort, "")
	if o := w.Outcome; o.RoomMiB != 896 || o.ReserveMiB != config.DefaultReserveMiB || !o.HeadroomOK || o.HeadroomMiB != testHeadroom.mib {
		t.Errorf("reading %+v; want room 896 of headroom %d", o, testHeadroom.mib)
	}
}

// A non-head with nothing to try makes no attempt and names what it is behind. Mutation: the
// no-attempt return leaves the reason unset, or elect does not note the ticket ahead.
func TestReasonBehindTicket(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	head := enqueue(t, e, need(2048))
	w := waitOutcome(t, enqueue(t, e, need(1)))
	wantReason(t, w, ReasonBehindTicket, "")
	if w.Position != 2 || w.Ahead != head.Name() || w.Evals != 0 {
		t.Errorf("position %d, ahead %q, %d evals; want 2, %q, none", w.Position, w.Ahead, w.Evals, head.Name())
	}
}

// A pass attempt that found the section busy reached no decision. Mutation: a busy section is
// section_busy whoever finds it.
func TestReasonBehindTicketWhenPassAttemptBusy(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	enqueue(t, e, need(2048))
	tk := enqueue(t, e, typical(1, 10))
	busySection(t)
	w := waitOutcome(t, tk)
	wantReason(t, w, ReasonBehindTicket, "")
	if w.Evals != 1 {
		t.Errorf("%d evals, want the attempt counted", w.Evals)
	}
}

// A busy section read nothing, so the numbers of the attempt before it are gone. Mutations: a busy
// section is behind_ticket whoever finds it; an attempt keeps the previous outcome's numbers.
func TestReasonSectionBusyHead(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	hold(t, e, need(128))
	tk := enqueue(t, e, need(2048))
	wantReason(t, waitOutcome(t, tk), ReasonRoomShort, "")
	clk.advance(time.Second)
	busySection(t)
	if w := waitOutcome(t, tk); w.Outcome != (Outcome{Reason: ReasonSectionBusy}) {
		t.Errorf("busy head: %+v, want section_busy and nothing else", w.Outcome)
	}
}

// A container request behind a head with no holds reaches passes; with the headroom unknown too,
// no typical run time is still the first answer. Mutation: exchange the first two cases of the
// switch in passes.
func TestReasonBehindHeadNoTypical(t *testing.T) {
	for _, ok := range []bool{true, false} {
		if !ok {
			unknownHeadroom(t)
		}
		e := newEngine(t, newStateDir(t, ""), newFakeClock())
		enqueue(t, e, need(2048))
		wantReason(t, waitOutcome(t, enqueue(t, e, container())), ReasonBehindHead, "no_typical")
	}
}

// One second over W is too long, with W as passes found it. Mutation: too_long set where the
// room falls short, or W not recorded.
func TestReasonBehindHeadTooLong(t *testing.T) {
	e, _ := backfillScene(t, false)
	w := waitOutcome(t, enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 81}))
	wantReason(t, w, ReasonBehindHead, "too_long")
	if w.Outcome.W != 80 || !w.Outcome.WKnown {
		t.Errorf("W %d (known %v), want 80", w.Outcome.W, w.Outcome.WKnown)
	}
}

// With no hold live as well, an unknown headroom is still the first answer. Mutation: exchange
// the headroom and no-holds cases of the switch in passes.
func TestNoPassHeadroomUnknown(t *testing.T) {
	unknownHeadroom(t)
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	enqueue(t, e, need(2048))
	wantReason(t, waitOutcome(t, enqueue(t, e, typical(1, 10))), ReasonBehindHead, "headroom_unknown")
}

// Mutation: the no-holds exit sets no_typical.
func TestNoPassNoHolds(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	enqueue(t, e, need(2048))
	wantReason(t, waitOutcome(t, enqueue(t, e, typical(1, 10))), ReasonBehindHead, "no_holds")
}

// The running job has no typical run time, so nothing says when the head's room comes. Mutation:
// the room-short exit of passes sets too_long, or marks W known.
func TestNoPassWaitUnknown(t *testing.T) {
	dir := capped(t, 2048)
	e := newEngine(t, dir, newFakeClock())
	hold(t, e, need(1024))
	enqueue(t, e, need(2048))
	w := waitOutcome(t, enqueue(t, e, typical(256, 10)))
	wantReason(t, w, ReasonBehindHead, "wait_unknown")
	if w.Outcome.WKnown {
		t.Errorf("W %d is known, want not", w.Outcome.W)
	}
}

// Mutation: the unreadable watch exit of passes sets wait_unknown, or marks W known.
func TestNoPassWatchUnreadable(t *testing.T) {
	e, dir := backfillScene(t, true)
	unreadableWatch(t, dir)
	w := waitOutcome(t, enqueue(t, e, typical(256, 10)))
	wantReason(t, w, ReasonBehindHead, "watch_unreadable")
	if w.Outcome.WKnown {
		t.Errorf("W %d is known, want not", w.Outcome.W)
	}
}

// Mutation: the level branch of watchWait sets watch_spacing.
func TestReasonWatchLevel(t *testing.T) {
	e := watched(t, watch.Detector{Level: watch.Yellow})
	w := waitOutcome(t, enqueue(t, e, need(128)))
	wantReason(t, w, ReasonWatchLevel, "")
	if w.Outcome.WatchGate != "checked" {
		t.Errorf("gate %q, want checked", w.Outcome.WatchGate)
	}
}

// Mutation: the spacing branch of watchWait sets watch_level.
func TestReasonWatchSpacing(t *testing.T) {
	e := watched(t, watch.Detector{YellowClearedNS: sampleWallNS - 1e9, LastAdmitNS: sampleWallNS - 1e9})
	wantReason(t, waitOutcome(t, enqueue(t, e, need(128))), ReasonWatchSpacing, "")
}

// The level is named before the calm that a level other than green also lacks. Mutation: test
// the calm first in restartRefusal.
func TestReasonRestartLevel(t *testing.T) {
	for level, gate := range map[watch.Level]string{watch.Yellow: "checked", watch.Unknown: "unknown"} {
		e := watched(t, watch.Detector{Level: level})
		w := waitOutcome(t, enqueue(t, e, restart(128)))
		wantReason(t, w, ReasonRestartLevel, "")
		if w.Outcome.WatchGate != gate {
			t.Errorf("level %v: gate %q, want %q", level, w.Outcome.WatchGate, gate)
		}
	}
}

// 4 s of the 10 s of calm have passed. Mutation: CalmLeftS left unset, or in ns.
func TestReasonRestartCalm(t *testing.T) {
	e := watched(t, watch.Detector{CalmSinceNS: sampleWallNS - 4e9})
	w := waitOutcome(t, enqueue(t, e, restart(128)))
	wantReason(t, w, ReasonRestartCalm, "")
	if w.Outcome.CalmLeftS != 6 {
		t.Errorf("calm left %v s, want 6", w.Outcome.CalmLeftS)
	}
}

// The room of 896 MiB covers 700, not 700 and the 256 of margin. Mutation: the margin clause of
// restartRefusal returns restart_calm.
func TestReasonRestartMargin(t *testing.T) {
	e := watched(t, watch.Detector{CalmSinceNS: sampleWallNS - 11e9})
	wantReason(t, waitOutcome(t, enqueue(t, e, restart(700))), ReasonRestartMargin, "")
}

// The outcome outlives Steps with no attempt and the election that rewrites t.last. Mutation: a
// `return t.last` of step not composed through the helper.
func TestOutcomeSurvivesElection(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	hold(t, e, need(128))
	tk := enqueue(t, e, need(2048))
	first := waitOutcome(t, tk)
	wantReason(t, first, ReasonRoomShort, "")
	clk.advance(100 * time.Millisecond) // inside the backoff: no election, no attempt
	if w := waitOutcome(t, tk); w != first {
		t.Errorf("between attempts: %+v, want %+v", w, first)
	}
	clk.advance(time.Second)
	w := waitOutcome(t, tk)
	wantReason(t, w, ReasonRoomShort, "")
	if w.Outcome != first.Outcome || w.Position != 1 {
		t.Errorf("after the election: %+v, want the outcome of %+v", w, first)
	}
}

// Evals counts attempts, not Steps. Mutation: Evals left unset, or incremented per Step.
func TestEvalsCount(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	hold(t, e, need(128))
	tk := enqueue(t, e, need(2048))
	for _, c := range []struct {
		after time.Duration
		want  int
	}{{0, 1}, {100 * time.Millisecond, 1}, {time.Second, 2}} {
		clk.advance(c.after)
		if w := waitOutcome(t, tk); w.Evals != c.want {
			t.Errorf("after %v: %d evals, want %d", c.after, w.Evals, c.want)
		}
	}
}

// What the admitting attempt read stays on the grant: the reading, no reason, and whether the
// watch state gated it. Mutations: the grant keeps no outcome; each gate set at the wrong branch.
func TestAdmissionReadingAndGate(t *testing.T) {
	for _, c := range []struct {
		name, gate string
		room       int64
		scene      func(*Engine, *fakeClock)
	}{
		{"no holds", "no_holds", 1024, func(*Engine, *fakeClock) {}},
		{"checked", "checked", 896, func(e *Engine, f *fakeClock) { hold(t, e, need(128)); setWatch(t, e, f, watch.Detector{}, 0) }},
		{"stale", "stale", 896, func(e *Engine, f *fakeClock) {
			hold(t, e, need(128))
			setWatch(t, e, f, watch.Detector{}, 10*time.Second)
		}},
		{"unknown", "unknown", 896, func(e *Engine, f *fakeClock) {
			hold(t, e, need(128))
			setWatch(t, e, f, watch.Detector{Level: watch.Unknown}, 0)
		}},
	} {
		f := newFakeClock()
		e := newEngine(t, newStateDir(t, ""), f)
		c.scene(e, f)
		o := admitted(t, step(t, enqueue(t, e, need(128)))).Admission()
		want := Outcome{HeadroomMiB: testHeadroom.mib, HeadroomOK: true, ReserveMiB: config.DefaultReserveMiB, RoomMiB: c.room, WatchGate: c.gate}
		if o != want {
			t.Errorf("%s: %+v, want %+v", c.name, o, want)
		}
	}
	e, _ := backfillScene(t, false)
	o := admitted(t, step(t, enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 80}))).Admission()
	if o.W != 80 || !o.WKnown || o.Reason != "" || o.NoPass != "" {
		t.Errorf("a pass: %+v, want W 80 and no reason", o)
	}
}
