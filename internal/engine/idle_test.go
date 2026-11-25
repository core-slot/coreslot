//go:build unix

package engine

import (
	"testing"
	"time"
)

// growingIdle is a head of needMiB that waited behind a hold while the headroom fell from 8000 to
// 6000 within 30 s, the room (headroom less the 1024 reserve) never fitting it, and the hold has
// just ended: the head's next attempt is by the idle rule.
func growingIdle(t *testing.T, needMiB int64) (*Engine, *Ticket, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	setLiveMem(t, 8000)
	g := hold(t, e, need(3000))
	h := enqueue(t, e, need(needMiB))
	for _, mib := range []int64{8000, 7000, 6000} {
		setLiveMem(t, mib)
		if isAdmitted(step(t, h)) {
			t.Fatal("the head started beside the hold")
		}
		clk.advance(15 * time.Second)
	}
	g.Close()
	return e, h, clk
}

// With no hold live, a head over the room waits while the headroom it read fell by more than 1 GiB
// within 60 s, and starts by the idle rule at the bound, 300 s after its first such attempt, even
// while the fall goes on; the admission says how long it waited. Mutations: no bound → it never
// starts; never recording the wait → idle_waited_s 0.
func TestIdleWaitsWhileOutsideGrowsUpToTheBound(t *testing.T) {
	_, h, clk := growingIdle(t, 6000)
	headroom := int64(6000)
	for waited := time.Duration(0); waited < idleWaitMax; waited += 20 * time.Second {
		if w := waitOutcome(t, h); w.Outcome.Reason != ReasonIdleGrowing {
			t.Fatalf("after %v: reason %q; want idle_growing", waited, w.Outcome.Reason)
		}
		headroom -= 700 // 2100 MiB a minute, on and on
		setLiveMem(t, headroom)
		clk.advance(20 * time.Second)
	}
	g := admitted(t, step(t, h))
	if s := g.Admission().IdleWaitedS; s < 300 || s > 320 {
		t.Errorf("idle_waited_s %v; want about 300", s)
	}
}

// Once the headroom has held for 60 s, the trend is calm and the head starts by the idle rule.
// Mutation: keeping every reading, not the last 60 s → it waits on, red.
func TestIdleStartsOnceTheFallIsOld(t *testing.T) {
	_, h, clk := growingIdle(t, 6000)
	if w := waitOutcome(t, h); w.Outcome.Reason != ReasonIdleGrowing {
		t.Fatalf("reason %q; want idle_growing", w.Outcome.Reason)
	}
	clk.advance(61 * time.Second)
	admitted(t, step(t, h))
}

// On a calm machine a head over the room starts at once, as the idle rule says.
// Mutation: waiting on need > room alone → it waits.
func TestIdleOverRoomCalmStartsAtOnce(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	setLiveMem(t, 2000)
	if g := admitted(t, step(t, enqueue(t, e, need(6000)))); g.Admission().IdleWaitedS != 0 {
		t.Errorf("idle_waited_s %v; want none", g.Admission().IdleWaitedS)
	}
}

// A head that fits the room starts at once whatever the trend. Mutation: dropping need > room →
// it waits.
func TestIdleHeadThatFitsIgnoresTheTrend(t *testing.T) {
	_, h, _ := growingIdle(t, 5000)
	setLiveMem(t, 6500) // a room of 5476 fits the 5000; the fall within 60 s is 1500
	admitted(t, step(t, h))
}

// The kernel's pressure above normal holds an idle start over the room; pressure that cannot be
// read holds nothing, so a system without the reading keeps today's idle start. Mutation: counting
// an unread pressure as not normal → the second waits.
func TestIdleWaitsAtPressure(t *testing.T) {
	for _, c := range []struct {
		level int
		read  bool
		waits bool
	}{{2, true, true}, {0, false, false}} {
		e := newEngine(t, newStateDir(t, ""), newFakeClock())
		setLiveMem(t, 2000)
		setPressure(t, c.level, c.read)
		r := step(t, enqueue(t, e, need(6000)))
		if isAdmitted(r) == c.waits {
			t.Errorf("pressure %d read %v: admitted %v; want %v", c.level, c.read, isAdmitted(r), !c.waits)
		}
	}
}
