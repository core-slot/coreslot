package state

import (
	"testing"
	"time"
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

// An observer that passes at the poll contract (at least every 2 s), with scheduling
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
