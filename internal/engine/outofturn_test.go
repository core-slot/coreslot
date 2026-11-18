//go:build unix

package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
)

// The engine's tests run at a normal kernel memory pressure level whatever the machine's, so that
// whether a waiter may go out of turn never depends on the machine; a test that needs another level
// sets it with setPressure.
func init() { kernelPressure = func() (int, bool) { return pressureNormal, true } }

// setPressure makes the kernel report the memory pressure level level for the rest of the test (ok
// false: a level that cannot be read). Going out of turn requires pressureNormal.
func setPressure(t *testing.T, level int, ok bool) {
	t.Helper()
	saved := kernelPressure
	kernelPressure = func() (int, bool) { return level, ok }
	t.Cleanup(func() { kernelPressure = saved })
}

// unknownHeadScene is a head that waits for memory outside coreslot: A (1024 MiB, no typical run
// time) holds, and the head B needs the whole 2048 MiB room, so nothing says when B can start: its
// W is unknown.
func unknownHeadScene(t *testing.T) (*Engine, *fakeClock, *Grant, *Ticket) {
	t.Helper()
	clk := newFakeClock()
	e := newEngine(t, capped(t, 2048), clk)
	a := hold(t, e, spec.Request{Label: "a", NeedMiB: 1024})
	clk.advance(time.Second) // A arrived before B: it is no run out of turn
	b := enqueue(t, e, spec.Request{Label: "b", NeedMiB: 2048})
	return e, clk, a, b
}

// watchSteps steps tk once a second, n times or until it is admitted, and reports the step that
// admitted it (1-based), 0 when none did.
func watchSteps(t *testing.T, clk *fakeClock, tk *Ticket, n int) int {
	t.Helper()
	for i := 1; i <= n; i++ {
		if isAdmitted(step(t, tk)) {
			return i
		}
		clk.advance(time.Second)
	}
	return 0
}

// A waiter that fits goes out of turn past a head whose wait is unknown once it has watched the
// head for its typical run time, and the head keeps its place. Mutation: dropping the out-of-turn
// branch of passes keeps C waiting behind B for good.
func TestOutOfTurnPassesAHeadWithAnUnknownWait(t *testing.T) {
	e, clk, _, b := unknownHeadScene(t)
	c := enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 5})
	w := waitOutcome(t, c)
	wantReason(t, w, ReasonBehindHead, "wait_unknown")
	clk.advance(time.Second)
	got := watchSteps(t, clk, c, 10)
	if got == 0 {
		t.Fatal("C never went out of turn past a head whose wait is unknown")
	}
	if got < 4 {
		t.Errorf("C went out of turn after watching B for about %d s, want at least its 5 s", got)
	}
	if g := c.grant; g.Admission().Pass != "turn" {
		t.Errorf("admission pass %q, want turn", g.Admission().Pass)
	}
	if p := position(t, step(t, b)); p != 1 {
		t.Errorf("B's position %d after C went out of turn, want 1", p)
	}
}

// Case 2 of 2026-11-18: B's wait is known and short, and C is a little longer, so C does not pass by
// duration; it goes out of turn once it has watched B for its typical run time. Mutations: dropping
// the out-of-turn branch keeps C out; recording "turn" for a pass by duration reddens the first.
func TestOutOfTurnPassesAShortKnownWait(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, capped(t, 2048), clk)
	hold(t, e, spec.Request{Label: "a", NeedMiB: 1024, TypicalS: 30})
	clk.advance(20 * time.Second)
	enqueue(t, e, spec.Request{Label: "b", NeedMiB: 2048})
	byDuration := admitted(t, step(t, enqueue(t, e, spec.Request{Label: "d", NeedMiB: 128, TypicalS: 10})))
	if p := byDuration.Admission().Pass; p != "duration" {
		t.Errorf("a pass by duration recorded %q, want duration", p)
	}
	c := enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 12})
	wantReason(t, waitOutcome(t, c), ReasonBehindHead, "out_of_turn_live")
	clk.advance(time.Second)
	byDuration.Close()
	if got := watchSteps(t, clk, c, 20); got == 0 {
		t.Fatal("C never went out of turn past a short known wait")
	}
}

// One run out of turn at a time: while C, which went out of turn, holds, D waits, however long it
// has watched B; once C ends, D goes. Mutation: dropping the arrived-after-the-head check admits D
// beside C.
func TestOutOfTurnOneAtATime(t *testing.T) {
	e, clk, _, _ := unknownHeadScene(t)
	c := enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 3})
	d := enqueue(t, e, spec.Request{Label: "d", NeedMiB: 256, TypicalS: 3})
	var cg *Grant
	for i := 0; i < 10 && cg == nil; i++ {
		if r := step(t, c); isAdmitted(r) {
			cg = r.(Admitted).Grant
		}
		if cg == nil {
			wantReason(t, waitOutcome(t, d), ReasonBehindHead, "wait_unknown")
		}
		clk.advance(time.Second)
	}
	if cg == nil {
		t.Fatal("C never went out of turn: the test shows nothing")
	}
	for i := 0; i < 5; i++ {
		wantReason(t, waitOutcome(t, d), ReasonBehindHead, "out_of_turn_live")
		clk.advance(time.Second)
	}
	cg.Close()
	if !isAdmitted(step(t, d)) {
		t.Error("D did not go out of turn once C ended")
	}
}

// A hold whose body does not parse hides whether it went out of turn, so nobody goes out of turn
// while one is live. Mutation: dropping the unreadable-hold check admits C.
func TestOutOfTurnNotBesideAnUnreadableHold(t *testing.T) {
	e, clk, a, _ := unknownHeadScene(t)
	c := enqueue(t, e, spec.Request{Label: "c", NeedMiB: 64, TypicalS: 6})
	for i := 0; i < 4; i++ {
		waitOutcome(t, c)
		clk.advance(time.Second)
	}
	if err := os.WriteFile(filepath.Join(e.st.Dir(), store.HoldsDir, a.HoldName()), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		wantReason(t, waitOutcome(t, c), ReasonBehindHead, "hold_unreadable")
		clk.advance(time.Second)
	}
}

// A head that fits is never passed out of turn, though it waits (here it simply has not stepped):
// its start, or the level gate's slot it waits for, is its own. Mutation: dropping the head-fits
// check admits C ahead of B.
func TestOutOfTurnNotWhileTheHeadFits(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, capped(t, 2048), clk)
	hold(t, e, spec.Request{Label: "a", NeedMiB: 512})
	b := enqueue(t, e, spec.Request{Label: "b", NeedMiB: 1024})
	c := enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 2})
	for i := 0; i < 5; i++ {
		wantReason(t, waitOutcome(t, c), ReasonBehindHead, "head_fits")
		clk.advance(time.Second)
	}
	if !isAdmitted(step(t, b)) {
		t.Error("B, which fits, did not start")
	}
}

// The bound is the time the waiter has watched the head, so an absence of its own (a sleeping
// machine, a stopped process) does not count. Mutation: judging by the head's created_ns admits C
// right after the gap.
func TestOutOfTurnCountsOnlyWatchedTime(t *testing.T) {
	e, clk, _, _ := unknownHeadScene(t)
	c := enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 5})
	waitOutcome(t, c)
	clk.advance(time.Hour)
	for i := 0; i < 3; i++ {
		wantReason(t, waitOutcome(t, c), ReasonBehindHead, "wait_unknown")
		clk.advance(time.Second)
	}
	if watchSteps(t, clk, c, 10) == 0 {
		t.Error("C never went out of turn once it had watched B long enough")
	}
}

// A restart head waits at least for the calm it still needs, so a waiter that ends within it may
// go out of turn before watching the head that long. Mutation: dropping the restart floor from the
// out-of-turn bound keeps C out.
func TestOutOfTurnRestartHeadKeepsItsCalm(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, capped(t, 2048), clk)
	hold(t, e, spec.Request{Label: "a", NeedMiB: 1024})
	clk.advance(time.Second)
	setWatch(t, e, clk, watch.Detector{CalmSinceNS: sampleWallNS}, 0) // green, calm from now: 10 s left
	enqueue(t, e, spec.Request{Label: "b", NeedMiB: 2048, Restart: true})
	if !isAdmitted(step(t, enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 8}))) {
		t.Error("C, which ends within the restart head's calm, did not go out of turn")
	}
}

// Behind a restart head whose wait is unknown, an unreadable watch state refuses the out-of-turn
// start, as it refuses a pass by duration. Mutation: ignoring the watch read error admits C.
func TestOutOfTurnRestartHeadUnreadableWatch(t *testing.T) {
	clk := newFakeClock()
	dir := capped(t, 2048)
	e := newEngine(t, dir, clk)
	hold(t, e, spec.Request{Label: "a", NeedMiB: 1024})
	clk.advance(time.Second)
	enqueue(t, e, spec.Request{Label: "b", NeedMiB: 2048, Restart: true})
	unreadableWatch(t, dir)
	c := enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 1})
	for i := 0; i < 4; i++ {
		wantReason(t, waitOutcome(t, c), ReasonBehindHead, "watch_unreadable")
		clk.advance(time.Second)
	}
}

// Going out of turn needs the kernel's memory pressure level to be normal: at warning or critical,
// or when the level cannot be read (as on Linux, which has none), nobody goes out of turn, though
// in-turn admission is unchanged. Mutation: dropping the pressure check admits C at warning.
func TestOutOfTurnOnlyAtNormalPressure(t *testing.T) {
	for _, c := range []struct {
		name  string
		level int
		ok    bool
		turn  bool
	}{{"normal", pressureNormal, true, true}, {"warning", 2, true, false}, {"critical", 4, true, false}, {"unreadable", 0, false, false}} {
		t.Run(c.name, func(t *testing.T) {
			e, clk, _, _ := unknownHeadScene(t)
			setPressure(t, c.level, c.ok)
			cc := enqueue(t, e, spec.Request{Label: "c", NeedMiB: 256, TypicalS: 2})
			if got := watchSteps(t, clk, cc, 6) != 0; got != c.turn {
				t.Fatalf("C went out of turn %v at pressure %d (read %v), want %v", got, c.level, c.ok, c.turn)
			}
			if !c.turn {
				wantReason(t, waitOutcome(t, cc), ReasonBehindHead, "pressure_not_normal")
			}
		})
	}
}
