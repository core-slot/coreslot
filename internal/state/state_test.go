package state

import (
	"reflect"
	"testing"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/spec"
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

func TestAheadSkipsDeadAndStalled(t *testing.T) {
	me := k(100, 10, 1)
	dead := Ticket{Key: k(10, 1, 1), Dead: true}
	stalled := Ticket{Key: k(20, 2, 1)}
	live := Ticket{Key: k(30, 3, 1)}
	behind := Ticket{Key: k(200, 4, 1)}

	obs := &Observer{}
	obs.Observe(stalled.Key, 7, 0)
	obs.Observe(live.Key, 7, 0)
	now := 10 * time.Second
	obs.Observe(live.Key, 8, now) // live changed; stalled did not

	// Dead and stalled are skipped: with only those ahead, me is head.
	if !Ahead([]Ticket{dead, stalled, {Key: me}, behind}, me, obs, now) {
		t.Error("only dead and stalled tickets ahead: want head")
	}
	// The first live, non-stalled ticket ends the scan: not head.
	if Ahead([]Ticket{dead, stalled, live, {Key: me}, behind}, me, obs, now) {
		t.Error("a live ticket ahead: want not head")
	}
	// Input order must not matter: the scan is in ticket order.
	if Ahead([]Ticket{behind, {Key: me}, live, stalled, dead}, me, obs, now) {
		t.Error("unsorted input with a live ticket ahead: want not head")
	}
	// A live ticket behind me never blocks me.
	if !Ahead([]Ticket{{Key: me}, behind}, me, obs, now) {
		t.Error("only tickets behind: want head")
	}
	// Alone.
	if !Ahead(nil, me, obs, now) {
		t.Error("empty queue: want head")
	}
	// Ties by pid then seq: same order key, smaller pid is ahead.
	tie := Ticket{Key: k(100, 5, 9)}
	obs.Observe(tie.Key, 1, now)
	if Ahead([]Ticket{tie, {Key: me}}, me, obs, now) {
		t.Error("equal order key, smaller pid ahead and live: want not head")
	}
}

func TestAheadStalledBecomesLiveAgainWhenObservedToChange(t *testing.T) {
	me := k(100, 10, 1)
	other := Ticket{Key: k(10, 1, 1)}
	obs := &Observer{}
	obs.Observe(other.Key, 5, 0)
	if Ahead([]Ticket{other, {Key: me}}, me, obs, 5*time.Second) {
		t.Error("fresh ticket ahead: want not head")
	}
	if !Ahead([]Ticket{other, {Key: me}}, me, obs, 10*time.Second) {
		t.Error("10 s without change: want head")
	}
	obs.Observe(other.Key, 6, 11*time.Second)
	if Ahead([]Ticket{other, {Key: me}}, me, obs, 12*time.Second) {
		t.Error("refresh observed: want not head again")
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
		obs.Pass()
		obs.Observe(key, 42, sec(s))
	}
	for s := 5; s < 30; s++ { // passes that stop before key
		obs.Pass()
	}
	obs.Pass()
	obs.Observe(key, 42, sec(30))
	if obs.Stalled(key, sec(30)) {
		t.Fatal("stalled on 4 s of watching and a 26 s gap nobody watched")
	}
	for s := 31; s <= 35; s++ {
		obs.Pass()
		obs.Observe(key, 42, sec(s))
		if obs.Stalled(key, sec(s)) {
			t.Fatalf("stalled at %d s after %d s of watching", s, 4+s-30)
		}
	}
	obs.Pass()
	obs.Observe(key, 42, sec(36))
	if !obs.Stalled(key, sec(36)) {
		t.Error("10 s of watching without a change across the gap: want stalled")
	}

	// A value that changed during the gap restarts the timer.
	other := k(2, 2, 2)
	obs.Pass()
	obs.Observe(other, 1, sec(100))
	for s := 101; s < 140; s++ {
		obs.Pass()
	}
	obs.Pass()
	obs.Observe(other, 2, sec(140))
	for s := 141; s <= 149; s++ {
		obs.Pass()
		obs.Observe(other, 2, sec(s))
	}
	if obs.Stalled(other, sec(149)) {
		t.Error("stalled 9 s after an observed change")
	}
}
