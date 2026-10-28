package state

import (
	"reflect"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
)

func k(order uint64, pid, seq int) Key { return Key{OrderKey: order, PID: pid, Seq: seq} }

func TestKeyLessOrdersByKeyThenPIDThenSeq(t *testing.T) {
	cases := []struct {
		a, b Key
		want bool
	}{
		{k(1, 9, 9), k(2, 1, 1), true},
		{k(2, 1, 1), k(1, 9, 9), false},
		{k(5, 1, 9), k(5, 2, 1), true},
		{k(5, 2, 1), k(5, 1, 9), false},
		{k(5, 2, 1), k(5, 2, 2), true},
		{k(5, 2, 2), k(5, 2, 1), false},
		{k(5, 2, 2), k(5, 2, 2), false},
	}
	for _, c := range cases {
		if got := c.a.Less(c.b); got != c.want {
			t.Errorf("%v.Less(%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// The election's verdict on one ticket; the scan itself is the engine's (its election tests).
func TestPassedOverSkipsDeadAndStalled(t *testing.T) {
	dead := Ticket{Key: k(10, 1, 1), Dead: true}
	stalled := Ticket{Key: k(20, 2, 1)}
	live := Ticket{Key: k(30, 3, 1)}

	obs := &Observer{}
	obs.Observe(stalled.Key, 7, 0)
	obs.Observe(live.Key, 7, 0)
	now := 10 * time.Second
	obs.Observe(live.Key, 8, now) // live changed; stalled did not

	if !PassedOver(dead, obs, now) {
		t.Error("dead ticket: want passed over")
	}
	if !PassedOver(stalled, obs, now) {
		t.Error("10 s without an observed change: want passed over")
	}
	if PassedOver(live, obs, now) {
		t.Error("a change observed: want not passed over")
	}
	if PassedOver(stalled, obs, now-time.Nanosecond) {
		t.Error("just under 10 s without a change: want not passed over")
	}
}

func TestObserverIgnoresClockValues(t *testing.T) {
	const future = int64(1) << 62
	const past = int64(1)
	for _, mtime := range []int64{future, past} {
		obs := &Observer{}
		key := k(1, 1, 1)
		obs.Observe(key, mtime, 100*time.Second)
		for s := 101; s < 110; s++ {
			obs.Observe(key, mtime, time.Duration(s)*time.Second)
			if obs.Stalled(key, time.Duration(s)*time.Second) {
				t.Fatalf("mtime %d stalled after %d s", mtime, s-100)
			}
		}
		obs.Observe(key, mtime, 110*time.Second)
		if !obs.Stalled(key, 110*time.Second) {
			t.Errorf("mtime %d unchanged for 10 s: want stalled", mtime)
		}
	}

	// One that changes every 2 s never stalls, however odd its values.
	obs := &Observer{}
	key := k(1, 1, 1)
	mtime := future
	for s := 0; s <= 120; s++ {
		now := time.Duration(s) * time.Second
		if s%2 == 0 {
			mtime--
		}
		obs.Observe(key, mtime, now)
		if obs.Stalled(key, now) {
			t.Fatalf("refreshed every 2 s but stalled at %d s", s)
		}
	}
}

func TestObserverDoesNotRestartTimerOnSameValue(t *testing.T) {
	obs := &Observer{}
	key := k(1, 1, 1)
	obs.Observe(key, 42, 0)
	for s := 1; s <= 9; s++ {
		obs.Observe(key, 42, time.Duration(s)*time.Second)
	}
	if !obs.Stalled(key, 10*time.Second) {
		t.Error("repeated sightings of an unchanged mtime must not restart the timer")
	}
	if obs.Stalled(key, 10*time.Second-1) {
		t.Error("9.999 s without change is not stalled")
	}
}

func TestFirstSightingStartsTimer(t *testing.T) {
	obs := &Observer{}
	key := k(1, 1, 1)
	// Never seen: cannot be judged stalled.
	if obs.Stalled(key, time.Hour) {
		t.Error("an unobserved ticket is not stalled")
	}
	obs.Observe(key, 99, 500*time.Second) // observer clock far from zero
	if obs.Stalled(key, 500*time.Second) || obs.Stalled(key, 509*time.Second) {
		t.Error("stalled before 10 s from first sight")
	}
	if !obs.Stalled(key, 510*time.Second) {
		t.Error("not stalled 10 s after first sight")
	}
}

func TestObserverForget(t *testing.T) {
	obs := &Observer{}
	key := k(1, 1, 1)
	obs.Observe(key, 1, 0)
	obs.Forget(key)
	if obs.Stalled(key, time.Hour) {
		t.Error("a forgotten ticket is unknown, not stalled")
	}
	// Reappearing with the same mtime is a first sighting again.
	obs.Observe(key, 1, 100*time.Second)
	if obs.Stalled(key, 105*time.Second) {
		t.Error("re-sighting after Forget must restart the timer")
	}
}

func TestEffectiveClampsOversize(t *testing.T) {
	need := map[string]spec.Range{
		"cpu":      {Min: 16, Max: 16},
		"pg-local": {Min: 1, Max: 1},
		"mem":      {Min: 2, Max: 8},
	}
	sizes := map[string]int{"cpu": 10, "pg-local": 2, "mem": 4}
	eff, clamped, undeclared := Effective(need, sizes)
	wantEff := map[string]spec.Range{
		"cpu":      {Min: 10, Max: 10},
		"pg-local": {Min: 1, Max: 1},
		"mem":      {Min: 2, Max: 4},
	}
	if !reflect.DeepEqual(eff, wantEff) {
		t.Errorf("eff = %v, want %v", eff, wantEff)
	}
	if want := map[string]int{"cpu": 16}; !reflect.DeepEqual(clamped, want) {
		t.Errorf("clamped = %v, want %v", clamped, want)
	}
	if len(undeclared) != 0 {
		t.Errorf("undeclared = %v, want none", undeclared)
	}
	if need["cpu"] != (spec.Range{Min: 16, Max: 16}) {
		t.Error("Effective mutated its input")
	}
}

func TestEffectiveMinEqualSizeIsNotClamped(t *testing.T) {
	eff, clamped, _ := Effective(map[string]spec.Range{"cpu": {Min: 10, Max: 12}}, map[string]int{"cpu": 10})
	if want := (spec.Range{Min: 10, Max: 10}); eff["cpu"] != want {
		t.Errorf("eff = %v, want %v", eff["cpu"], want)
	}
	if len(clamped) != 0 {
		t.Errorf("min == size must not be clamped, got %v", clamped)
	}
}

func TestEffectiveMarksUndeclared(t *testing.T) {
	need := map[string]spec.Range{
		"pg-local": {Min: 1, Max: 1},
		"cpu":      {Min: 2, Max: 4},
		"alpha":    {Min: 1, Max: 3},
	}
	eff, clamped, undeclared := Effective(need, map[string]int{"cpu": 8})
	if want := []string{"alpha", "pg-local"}; !reflect.DeepEqual(undeclared, want) {
		t.Errorf("undeclared = %v, want %v (sorted)", undeclared, want)
	}
	if _, ok := eff["pg-local"]; ok {
		t.Error("an undeclared resource must be excluded from eff")
	}
	if _, ok := clamped["pg-local"]; ok {
		t.Error("an undeclared resource is never clamped")
	}
	if want := (spec.Range{Min: 2, Max: 4}); eff["cpu"] != want || len(eff) != 1 {
		t.Errorf("eff = %v, want only cpu %v", eff, want)
	}
}

func TestPlanCountsAllLocks(t *testing.T) {
	sizes := map[string]int{"cpu": 12}
	eff := map[string]spec.Range{"cpu": {Min: 1, Max: 12}}

	// After a shrink: size 12, a hold on indices 10-13 gives locked 4 (I3).
	take, ok := Plan(eff, sizes, map[string]int{"cpu": 4})
	if !ok || take["cpu"] != 8 {
		t.Errorf("size 12 locked 4: take = %v ok = %v, want cpu 8", take, ok)
	}
	// Locked equal to size: nothing free.
	if take, ok := Plan(eff, sizes, map[string]int{"cpu": 12}); ok || take != nil {
		t.Errorf("size 12 locked 12: take = %v ok = %v, want not ok", take, ok)
	}
	// Locks above the size can drive free below zero; still not ok, never a negative take.
	if take, ok := Plan(eff, sizes, map[string]int{"cpu": 14}); ok || take != nil {
		t.Errorf("locked above size: take = %v ok = %v, want not ok", take, ok)
	}
	// 4..12 with 5 free takes 5.
	take, ok = Plan(map[string]spec.Range{"cpu": {Min: 4, Max: 12}}, sizes, map[string]int{"cpu": 7})
	if !ok || take["cpu"] != 5 {
		t.Errorf("4..12 with 5 free: take = %v ok = %v, want cpu 5", take, ok)
	}
	// Free below min.
	if _, ok := Plan(map[string]spec.Range{"cpu": {Min: 6, Max: 12}}, sizes, map[string]int{"cpu": 7}); ok {
		t.Error("free 5 below min 6: want not ok")
	}
	// Max below free caps the take; a resource with no entry in locked has none locked.
	take, ok = Plan(map[string]spec.Range{"cpu": {Min: 2, Max: 3}}, sizes, nil)
	if !ok || take["cpu"] != 3 {
		t.Errorf("2..3 on free 12: take = %v ok = %v, want cpu 3", take, ok)
	}
}

func TestPlanAllResourcesMustFit(t *testing.T) {
	eff := map[string]spec.Range{"cpu": {Min: 2, Max: 4}, "pg-local": {Min: 1, Max: 1}}
	sizes := map[string]int{"cpu": 8, "pg-local": 1}
	take, ok := Plan(eff, sizes, map[string]int{"cpu": 1})
	if !ok || take["cpu"] != 4 || take["pg-local"] != 1 {
		t.Errorf("take = %v ok = %v", take, ok)
	}
	if take, ok := Plan(eff, sizes, map[string]int{"pg-local": 1}); ok || take != nil {
		t.Errorf("one resource without room: take = %v ok = %v, want not ok", take, ok)
	}
}

// A waiter behind live tickets plans against free less every minimum of every ticket ahead, not
// only the minimums of the resources the head is short of; its extra pass stops there too.
//
// Mutation: PlanBehind subtracting only the minimums of the resources the head is short of (free
// below its minimum) admits the cpu 5 waiter.
func TestPlanBehindSubtractsEveryMinimum(t *testing.T) {
	sizes := map[string]int{"cpu": 14, "mem": 76}
	locked := map[string]int{"mem": 10}
	head := map[string]spec.Range{"cpu": {Min: 10, Max: 10}, "mem": {Min: 70, Max: 70}} // short of mem only
	ahead := []map[string]spec.Range{head}
	if _, ok := Plan(head, sizes, locked); ok {
		t.Fatal("the head fits: the case does not test a head short of memory")
	}
	take, ok := PlanBehind(map[string]spec.Range{"cpu": {Min: 1, Max: 1}}, sizes, locked, ahead)
	if !ok || take["cpu"] != 1 {
		t.Errorf("cpu 1 behind the head: take = %v ok = %v, want cpu 1", take, ok)
	}
	if take, ok := PlanBehind(map[string]spec.Range{"cpu": {Min: 5, Max: 5}}, sizes, locked, ahead); ok {
		t.Errorf("cpu 5 with 14 free behind the head's cpu minimum of 10: take = %v, want not ok", take)
	}
	if take, ok := PlanBehind(map[string]spec.Range{"mem": {Min: 10, Max: 10}}, sizes, locked, ahead); ok {
		t.Errorf("mem 10 behind a head short of mem: take = %v, want not ok", take)
	}
	// The extra pass leaves the minimums ahead free: 1..14 takes the 4 above the head's 10.
	if take, ok := PlanBehind(map[string]spec.Range{"cpu": {Min: 1, Max: 14}}, sizes, locked, ahead); !ok || take["cpu"] != 4 {
		t.Errorf("cpu 1..14 behind the head: take = %v ok = %v, want cpu 4", take, ok)
	}
	// With nothing ahead it is Plan.
	eff := map[string]spec.Range{"cpu": {Min: 5, Max: 14}, "mem": {Min: 10, Max: 70}}
	want, _ := Plan(eff, sizes, locked)
	if take, ok := PlanBehind(eff, sizes, locked, nil); !ok || !reflect.DeepEqual(take, want) {
		t.Errorf("nothing ahead: take = %v ok = %v, want Plan's %v", take, ok, want)
	}
}

// The rule's reach: a waiter passes a head short of a resource the waiter does not name (a
// database slot another run holds), never one short of a resource it names.
//
// Mutation: PlanBehind refusing a waiter when any resource a ticket ahead names has free below
// that ticket's minimum refuses the cpu-only waiter.
func TestPlanBehindPassesAHeadShortOfAnotherResource(t *testing.T) {
	sizes := map[string]int{"cpu": 14, "pg-local": 1}
	locked := map[string]int{"pg-local": 1} // held by another run
	ahead := []map[string]spec.Range{{"cpu": {Min: 2, Max: 2}, "pg-local": {Min: 1, Max: 1}}}
	if take, ok := PlanBehind(map[string]spec.Range{"cpu": {Min: 1, Max: 1}}, sizes, locked, ahead); !ok || take["cpu"] != 1 {
		t.Errorf("cpu 1 behind a head waiting for pg-local: take = %v ok = %v, want cpu 1", take, ok)
	}
	both := map[string]spec.Range{"cpu": {Min: 1, Max: 1}, "pg-local": {Min: 1, Max: 1}}
	if take, ok := PlanBehind(both, sizes, locked, ahead); ok {
		t.Errorf("cpu 1 and pg-local 1 behind a head waiting for pg-local: take = %v, want not ok", take)
	}
}

func TestPlanCapsGrantAt512(t *testing.T) {
	// PROTOCOL.md section 12.1: every minimum first, then extra in ascending resource name while
	// the grant holds fewer than 512 tokens.
	eff := map[string]spec.Range{
		"gpu": {Min: 100, Max: 256},
		"cpu": {Min: 100, Max: 256},
		"db":  {Min: 100, Max: 256},
	}
	sizes := map[string]int{"cpu": 300, "db": 300, "gpu": 300}
	take, ok := Plan(eff, sizes, nil)
	want := map[string]int{"cpu": 256, "db": 156, "gpu": 100}
	if !ok || !reflect.DeepEqual(take, want) {
		t.Errorf("take = %v ok = %v, want %v", take, ok, want)
	}
	// Below the cap nothing changes: each resource gets min(max, free).
	take, ok = Plan(map[string]spec.Range{"cpu": {Min: 1, Max: 200}, "db": {Min: 1, Max: 200}},
		map[string]int{"cpu": 300, "db": 150}, map[string]int{"cpu": 10})
	if want := map[string]int{"cpu": 200, "db": 150}; !ok || !reflect.DeepEqual(take, want) {
		t.Errorf("take = %v ok = %v, want %v", take, ok, want)
	}
}

func TestAdoptPlanHasNoMinimum(t *testing.T) {
	// PROTOCOL.md section 21.7: size 10, 8 locked, need 3..3: takes the 2 free.
	eff := map[string]spec.Range{"cpu": {Min: 3, Max: 3}}
	if take := AdoptPlan(eff, map[string]int{"cpu": 10}, map[string]int{"cpu": 8}); take["cpu"] != 2 {
		t.Errorf("take = %v, want cpu 2", take)
	}
	// Locks above the size never make free negative (I3).
	if take := AdoptPlan(eff, map[string]int{"cpu": 10}, map[string]int{"cpu": 12}); take["cpu"] != 0 {
		t.Errorf("locked above size: take = %v, want cpu 0", take)
	}
	// The minimum pass covers every resource before the extra pass, under the 512 cap.
	eff = map[string]spec.Range{
		"a": {Min: 200, Max: 256}, "b": {Min: 200, Max: 256}, "c": {Min: 100, Max: 256},
	}
	sizes := map[string]int{"a": 300, "b": 300, "c": 300}
	take := AdoptPlan(eff, sizes, map[string]int{"c": 250})
	if want := map[string]int{"a": 256, "b": 206, "c": 50}; !reflect.DeepEqual(take, want) {
		t.Errorf("take = %v, want %v", take, want)
	}
}

// With passes, only time between examinations in consecutive passes counts: a ticket left out
// of some passes is not judged STALLED on the unwatched interval, and the watched time before
// the gap still counts.
func TestObserverCountsOnlyConsecutiveExaminations(t *testing.T) {
	obs := &Observer{}
	key := k(1, 1, 1)
	sec := func(s int) time.Duration { return time.Duration(s) * time.Second }
	for s := 0; s <= 4; s++ { // watched 0..4 s
		obs.Pass(sec(s))
		obs.Observe(key, 42, sec(s))
	}
	for s := 5; s < 30; s++ { // passes that stop before key
		obs.Pass(sec(s))
	}
	obs.Pass(sec(30))
	obs.Observe(key, 42, sec(30))
	if obs.Stalled(key, sec(30)) {
		t.Fatal("stalled on 4 s of watching and a 26 s gap nobody watched")
	}
	for s := 31; s <= 35; s++ {
		obs.Pass(sec(s))
		obs.Observe(key, 42, sec(s))
		if obs.Stalled(key, sec(s)) {
			t.Fatalf("stalled at %d s after %d s of watching", s, 4+s-30)
		}
	}
	obs.Pass(sec(36))
	obs.Observe(key, 42, sec(36))
	if !obs.Stalled(key, sec(36)) {
		t.Error("10 s of watching without a change across the gap: want stalled")
	}

	// A value that changed during the gap restarts the timer.
	other := k(2, 2, 2)
	obs.Pass(sec(100))
	obs.Observe(other, 1, sec(100))
	for s := 101; s < 140; s++ {
		obs.Pass(sec(s))
	}
	obs.Pass(sec(140))
	obs.Observe(other, 2, sec(140))
	for s := 141; s <= 149; s++ {
		obs.Pass(sec(s))
		obs.Observe(other, 2, sec(s))
	}
	if obs.Stalled(other, sec(149)) {
		t.Error("stalled 9 s after an observed change")
	}
}

// The observer's own absence is not watched (PROTOCOL.md section 10.2): an interval of more than
// MaxPassGap between two consecutive passes adds no watched time to any ticket, one of at most
// MaxPassGap adds it as usual, and a ticket that stopped while the observer was away is STALLED
// only after StallAfter of watched time once the observer is back.
func TestObserverDoesNotCountItsOwnAbsence(t *testing.T) {
	sec := func(s int) time.Duration { return time.Duration(s) * time.Second }
	obs := &Observer{}
	same, changed := k(1, 1, 1), k(2, 2, 2)
	pass := func(s int, changedValue int64) {
		obs.Pass(sec(s))
		obs.Observe(same, 42, sec(s))
		obs.Observe(changed, changedValue, sec(s))
	}
	for s := 0; s <= 4; s++ { // 1 s passes: 4 s watched
		pass(s, 1)
	}
	pass(6, 1) // a 2 s gap is watched: 6 s
	// The observer is away for 15 s; meanwhile changed refreshes once more and stops.
	pass(21, 2)
	if obs.Stalled(same, sec(21)) {
		t.Fatal("same is STALLED right after the observer's 15 s absence: the absence was counted as watched")
	}
	for s := 22; s <= 24; s++ { // 7..9 s watched
		pass(s, 2)
		if obs.Stalled(same, sec(s)) {
			t.Fatalf("same is STALLED at %d s after %d s of watching", s, 6+s-21)
		}
	}
	pass(25, 2)
	if !obs.Stalled(same, sec(25)) {
		t.Error("same unchanged for 10 s of watching (6 before the absence, 4 after): want STALLED")
	}
	for s := 26; s <= 30; s++ {
		pass(s, 2)
		if obs.Stalled(changed, sec(s)) {
			t.Fatalf("changed is STALLED at %d s, %d s after its change was observed", s, s-21)
		}
	}
	pass(31, 2)
	if !obs.Stalled(changed, sec(31)) {
		t.Error("changed unchanged for 10 s of watching after the absence: want STALLED")
	}
}

// An observer that passes at the library's poll contract (at least every 2 s), with scheduling
// jitter on top, keeps watching: gaps of 2.0 to 2.4 s all count, so a stopped ticket goes STALLED
// once 10 s of them have passed, and not before.
func TestObserverWatchesAtThePollContract(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	obs := &Observer{}
	key := k(1, 1, 1)
	obs.Pass(0)
	obs.Observe(key, 42, 0)
	now := time.Duration(0)
	for i, gap := range []time.Duration{ms(2200), ms(2400), ms(2000), ms(2300), ms(2100), ms(2200)} {
		now += gap
		obs.Pass(now)
		obs.Observe(key, 42, now)
		if want := now >= StallAfter; obs.Stalled(key, now) != want {
			t.Fatalf("pass %d at %v (gap %v): STALLED %v, want %v", i+1, now, gap, !want, want)
		}
	}
	if !obs.Stalled(key, now) {
		t.Fatalf("not STALLED after %v of passes 2.0-2.4 s apart", now)
	}
}

// The watched-gap boundary, in literals so that a change to MaxPassGap reddens it: gaps of 2.9 s
// and of exactly 3 s between passes are watched, a gap of 3 s + 1 ms is not.
func TestObserverPassGapBoundary(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	for _, c := range []struct {
		gap     time.Duration
		watched bool
	}{{ms(2900), true}, {ms(3000), true}, {ms(3001), false}} {
		obs := &Observer{}
		key := k(1, 1, 1)
		obs.Pass(0)
		obs.Observe(key, 42, 0)
		now := time.Duration(0)
		for range 4 { // 11.6 s, 12 s or 12.004 s in all
			now += c.gap
			obs.Pass(now)
			obs.Observe(key, 42, now)
		}
		if got := obs.Stalled(key, now); got != c.watched {
			t.Errorf("four gaps of %v (%v in all): STALLED %v, want %v", c.gap, now, got, c.watched)
		}
	}
}
