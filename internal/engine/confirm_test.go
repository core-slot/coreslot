//go:build unix

package engine

import (
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// The engine's tests confirm on the written footprints, as if the process table could not be
// read, so that no test depends on the trees of the machine it runs on; a test of the confirmation
// sets the measurement with measure.
func init() {
	treeFootprints = func([]memsample.Root) (map[int]int64, bool) { return nil, false }
}

// measure makes the confirmation measure the trees mib gives by pid (ok false: the process table
// cannot be read), calling between, when set, between the two headroom readings. It returns the
// number of measurements made.
func measure(t *testing.T, mib map[int]int64, ok bool, between func()) *int {
	t.Helper()
	calls := new(int)
	saved := treeFootprints
	treeFootprints = func(roots []memsample.Root) (map[int]int64, bool) {
		*calls++
		if between != nil {
			between()
		}
		if !ok {
			return nil, false
		}
		out := map[int]int64{}
		for _, r := range roots {
			if v, in := mib[r.PID]; in {
				out[r.PID] = v
			}
		}
		return out, true
	}
	t.Cleanup(func() { treeFootprints = saved })
	return calls
}

// otherHold writes and locks another client's live hold of need whose run last wrote footprint,
// and returns its pid.
func otherHold(t *testing.T, dir string, need, footprint int64) int {
	t.Helper()
	pid := 4000 + int(otherHolds.Add(1))
	lockHere(t, writeHold(t, dir, wire.Hold{Label: "other", PID: pid, NeedMiB: need, FootprintMiB: footprint}))
	return pid
}

// A hold whose run wrote 3072 MiB of its 4096 and whose tree now holds nothing has freed memory the
// headroom already counts: the 3072 MiB the room test admits on the written footprint is refused,
// with the confirmed room, and the refusal records no spacing slot. Mutations: skip the
// confirmation (admitted); confirm after the level gate's record (last_admit_ns set).
func TestConfirmationRefusesMemoryAHoldFreed(t *testing.T) {
	dir := capped(t, 6144)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	a := otherHold(t, dir, 4096, 3072)
	setWatch(t, e, clk, watch.Detector{YellowClearedNS: sampleWallNS - 1e9}, 0)
	measure(t, map[int]int64{a: 0}, true, nil)
	w := waitOutcome(t, enqueue(t, e, need(3072)))
	if w.Outcome.Reason != ReasonRoomShort || w.Outcome.RoomMiB != 2048 || w.Outcome.Pass != "" {
		t.Errorf("%+v; want room_short in a confirmed room of 2048 MiB, no pass", w.Outcome)
	}
	if d, _, _, _ := e.st.ReadWatch(); d.LastAdmitNS != 0 {
		t.Errorf("a refused confirmation recorded a start: %d", d.LastAdmitNS)
	}
}

// A refused confirmation reports no pass, though the waiter had passed the head by duration.
// Mutation: leave the pass set on refusal (duration).
func TestARefusedConfirmationClearsThePass(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, capped(t, 2048), clk)
	hold(t, e, spec.Request{Label: "a", NeedMiB: 1024, TypicalS: 30})
	clk.advance(20 * time.Second)
	enqueue(t, e, spec.Request{Label: "b", NeedMiB: 2048})
	measure(t, nil, true, func() { testHeadroom.mib -= 2048 })
	w := waitOutcome(t, enqueue(t, e, spec.Request{Label: "d", NeedMiB: 128, TypicalS: 10}))
	if w.Outcome.Reason != ReasonRoomShort || w.Outcome.Pass != "" {
		t.Errorf("%+v; want room_short and no pass", w.Outcome)
	}
}

// One hold freed memory and another took it before either wrote again: the headroom never moved,
// yet the written room is 6144 MiB and the room at this instant 3072. A 4096 MiB request is refused
// in the confirmed room. Mutation: confirm on a fresh headroom with the written footprints
// (admitted in a room of 6144).
func TestConfirmationSeesMemoryMovedBetweenHolds(t *testing.T) {
	dir := capped(t, 8192)
	e := newEngine(t, dir, newFakeClock())
	a := otherHold(t, dir, 4096, 3072)
	b := otherHold(t, dir, 4096, 3072)
	measure(t, map[int]int64{a: 0, b: 3072}, true, nil)
	w := waitOutcome(t, enqueue(t, e, need(4096)))
	if w.Outcome.Reason != ReasonRoomShort || w.Outcome.RoomMiB != 3072 {
		t.Errorf("%+v; want room_short in a confirmed room of 3072 MiB", w.Outcome)
	}
}

// Memory taken between the two headroom readings counts: the lower reading builds the room.
// Mutation: confirm on the first reading alone (admitted).
func TestConfirmationTakesTheLowerHeadroom(t *testing.T) {
	dir := capped(t, 6144)
	e := newEngine(t, dir, newFakeClock())
	a := otherHold(t, dir, 4096, 3072)
	measure(t, map[int]int64{a: 3072}, true, func() { testHeadroom.mib -= 3072 })
	if w := waitOutcome(t, enqueue(t, e, need(3072))); w.Outcome.Reason != ReasonRoomShort || w.Outcome.RoomMiB != 2048 {
		t.Errorf("%+v; want room_short in a confirmed room of 2048 MiB", w.Outcome)
	}
}

// A hold the measurement has no entry for, and every hold when the process table cannot be read,
// keeps its written footprint: 3072 MiB fits the written room of 5120. Mutations: count an
// unmeasured hold at 0 (refused); refuse when the table cannot be read (refused).
func TestAnUnmeasuredHoldKeepsItsWrittenFootprint(t *testing.T) {
	for _, listed := range []bool{true, false} {
		dir := capped(t, 6144)
		e := newEngine(t, dir, newFakeClock())
		otherHold(t, dir, 4096, 3072)
		measure(t, map[int]int64{}, listed, nil)
		if g := admitted(t, step(t, enqueue(t, e, need(3072)))); g.Admission().RoomMiB != 5120 {
			t.Errorf("listing ok %v: room %d, want the written 5120", listed, g.Admission().RoomMiB)
		}
	}
}

// The confirmation only refuses: a request the written footprints keep out stays out though the
// trees measured now would leave it room, and nothing is measured. Mutation: confirm in place of the
// room test (admitted).
func TestConfirmationNeverAdmitsWhatTheRoomRefused(t *testing.T) {
	dir := capped(t, 6144)
	e := newEngine(t, dir, newFakeClock())
	a := otherHold(t, dir, 4096, 0)
	calls := measure(t, map[int]int64{a: 4096}, true, nil)
	if w := waitOutcome(t, enqueue(t, e, need(3072))); w.Outcome.Reason != ReasonRoomShort || *calls != 0 {
		t.Errorf("%+v after %d measurements; want room_short and none", w.Outcome, *calls)
	}
}

// An admission reports the room it was confirmed in: a hold measured above its written footprint
// leaves more room than the written one. Mutation: keep the room test's values in the outcome
// (2048).
func TestAnAdmissionCarriesTheConfirmedRoom(t *testing.T) {
	dir := capped(t, 6144)
	e := newEngine(t, dir, newFakeClock())
	a := otherHold(t, dir, 4096, 0)
	measure(t, map[int]int64{a: 2048}, true, nil)
	if g := admitted(t, step(t, enqueue(t, e, need(1024)))); g.Admission().RoomMiB != 4096 {
		t.Errorf("room %d, want the confirmed 4096", g.Admission().RoomMiB)
	}
}

// Nothing is measured with no hold live, for a container request, or for a forced start beside a
// live hold. Mutations: drop the guard (a measurement with no hold, and for the container request);
// confirm in the forced admission's publication (a measurement).
func TestNoConfirmationWhereNoneIsDue(t *testing.T) {
	dir := capped(t, 2048)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	calls := measure(t, nil, true, nil)
	hold(t, e, need(512))
	if *calls != 0 {
		t.Errorf("%d measurements for an admission with no hold live, want none", *calls)
	}
	admitted(t, step(t, enqueue(t, e, spec.Request{Label: "c", NeedMiB: 512, RealMemory: true})))
	if *calls != 0 {
		t.Errorf("%d measurements for a container request, want none", *calls)
	}
	tk := enqueue(t, e, need(4096))
	requestStart(t, dir, clk, tk)
	admitted(t, step(t, tk))
	if *calls != 0 {
		t.Errorf("%d measurements for a forced start beside a live hold, want none", *calls)
	}
}
