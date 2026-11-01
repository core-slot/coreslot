//go:build unix

package engine

import (
	"reflect"
	"testing"
	"time"
)

// Every engine test reads no headroom unless it sets one, so that no mem admission, wait or
// clamp depends on the test machine's memory. Helper processes run this init too.
func init() {
	liveMem.headroom = func() (int64, bool) { return 0, false }
	liveMem.use = func(map[int]int64, map[int]bool) map[int]int64 { return nil }
}

// setLiveMem replaces the live capacity's readings for one test: the headroom in MiB, and the use in
// KiB that every root asked about reports (none when useKiB is negative). It returns the number
// of headroom reads.
func setLiveMem(t *testing.T, headroomMiB, useKiB int64) *int {
	t.Helper()
	saved := liveMem
	t.Cleanup(func() { liveMem = saved })
	reads := new(int)
	liveMem.headroom = func() (int64, bool) {
		*reads++
		return headroomMiB, true
	}
	liveMem.use = func(roots map[int]int64, _ map[int]bool) map[int]int64 {
		if useKiB < 0 {
			return nil
		}
		out := make(map[int]int64, len(roots))
		for pid := range roots {
			out[pid] = useKiB
		}
		return out
	}
	return reads
}

const staticCfg = "[resources]\nmem = \"1G\"\n"       // 1024 MiB
const liveCfg = staticCfg + "[memory]\nlive = true\n" // the live capacity is off unless turned on

// A need above what the static size leaves free is admitted from the headroom: 4096 MiB less A's
// unmeasured 1 GiB is 3 GiB on top of A's 1 GiB. Mutation: capacityFor returning the size
// unchanged, and B waits.
func TestLiveAdmitsAboveStatic(t *testing.T) {
	dir := newStateDir(t, liveCfg)
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 4096, -1)
	admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	g := admitted(t, step(t, enqueue(t, e, need("mem=2G"))))
	if g.GrantMiB() != 2048 || !g.LiveMem() || g.ClampedMiB() != 0 {
		t.Errorf("grant %d, LiveMem %v, clamped %d; want 2048 from a live capacity, not clamped",
			g.GrantMiB(), g.LiveMem(), g.ClampedMiB())
	}
}

// A need the static size covers never reads the headroom. Mutation: computing the live capacity
// unconditionally, and the read count is 2.
func TestLiveNotReadWhenStaticSuffices(t *testing.T) {
	dir := newStateDir(t, liveCfg)
	e := newEngine(t, dir, newFakeClock())
	reads := setLiveMem(t, 4096, -1)
	admitted(t, step(t, enqueue(t, e, need("mem=512M"))))
	g := admitted(t, step(t, enqueue(t, e, need("mem=512M"))))
	if *reads != 0 || g.LiveMem() {
		t.Errorf("%d headroom reads, LiveMem %v; want none and false", *reads, g.LiveMem())
	}
}

// [memory] live = false keeps the static size. Mutation: capacityFor ignoring LiveMem.
func TestLiveOffByConfig(t *testing.T) {
	dir := newStateDir(t, staticCfg+"[memory]\nlive = false\n")
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 4096, -1)
	admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	if r := step(t, enqueue(t, e, need("mem=2G"))); isAdmitted(r) {
		t.Fatal("mem=2G admitted with live = false and the static size held")
	}
}

// A waiter whose need only the live capacity would admit does not pass a live ticket ahead that
// is short on the configured size: H waits for more than the headroom, and P's 384 MiB would come
// from the headroom, taking what H waits for. Withdrawn, H no longer holds P back. Mutation: drop
// the live test from the passer's checks in admit, and P is admitted past H.
func TestLivePasserDoesNotPass(t *testing.T) {
	dir := newStateDir(t, liveCfg)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	setLiveMem(t, 4096, -1)
	admitted(t, step(t, enqueue(t, e, need("mem=768M"))))
	h := enqueue(t, e, need("mem=5000M"))
	p := enqueue(t, e, need("mem=384M"))
	if r := step(t, h); isAdmitted(r) {
		t.Fatal("H admitted beside A")
	}
	if r := step(t, p); isAdmitted(r) {
		t.Fatal("P admitted past H from the live size")
	}
	if err := h.Withdraw(); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second)
	if g := admitted(t, step(t, p)); !g.LiveMem() {
		t.Errorf("P, alone, granted %d MiB without the live capacity", g.GrantMiB())
	}
}

// A headroom that cannot be read keeps the static size, without an error. Mutation: using the
// reading whatever ok says, and B is admitted.
func TestLiveReadFailsIsStatic(t *testing.T) {
	dir := newStateDir(t, liveCfg)
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 4096, -1)
	liveMem.headroom = func() (int64, bool) { return 4096, false }
	admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	if r := step(t, enqueue(t, e, need("mem=2G"))); isAdmitted(r) {
		t.Fatal("mem=2G admitted from a headroom that was not read")
	}
}

// A hold without tree counts its whole reservation: 2560 MiB less A's 1 GiB leaves 1536 MiB, too
// little for B's 2 GiB. With A's tree measured at 1 GiB nothing of it is still to grow, 2560 MiB
// are free and B is admitted. Mutation: measure taking every hold as claiming its tree reddens
// the first half; never asking use reddens the second.
func TestLiveUnmeasuredHoldCountsWhole(t *testing.T) {
	for _, c := range []struct {
		name  string
		tree  bool
		admit bool
	}{{"no tree", false, false}, {"tree measured", true, true}} {
		t.Run(c.name, func(t *testing.T) {
			dir := newStateDir(t, liveCfg)
			e := newEngine(t, dir, newFakeClock())
			setLiveMem(t, 2560, 1<<20)
			req := need("mem=1G")
			req.Tree = c.tree
			admitted(t, step(t, enqueue(t, e, req)))
			if r := step(t, enqueue(t, e, need("mem=2G"))); isAdmitted(r) != c.admit {
				t.Errorf("B admitted %v, want %v", isAdmitted(r), c.admit)
			}
		})
	}
}

// A need above the live capacity is clamped to it: 3072 MiB free, not the static 1024. Mutation:
// clamping to the configured size, and the grant is 1024.
func TestLiveClampAgainstLiveSize(t *testing.T) {
	dir := newStateDir(t, liveCfg)
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 3072, -1)
	g := admitted(t, step(t, enqueue(t, e, need("mem=4G"))))
	if g.GrantMiB() != 3072 || g.ClampedMiB() != 4096 || !g.LiveMem() {
		t.Errorf("grant %d, clamped %d, LiveMem %v; want 3072, clamped 4096, true",
			g.GrantMiB(), g.ClampedMiB(), g.LiveMem())
	}
}

// B's process runs inside A's tree and B's whole reservation counts, so A's tree is read without
// B's subtree; C, a tree hold that reserves nothing, stops the walk unread; D and E name one pid
// and both count whole. Mutation: stop only at the measured roots, and 12 is no stop; roots
// without the h.needMiB > 0 check, and 14 is a root; claims[h.pid] == 1 as >= 1, and 16 is a root.
func TestMeasureStopsAtEveryLiveHold(t *testing.T) {
	setLiveMem(t, 0, -1)
	var roots map[int]int64
	var stop map[int]bool
	liveMem.use = func(r map[int]int64, s map[int]bool) map[int]int64 {
		roots, stop = r, s
		return nil
	}
	measure([]memHold{{pid: 10, tree: true, needMiB: 4096}, {pid: 12, needMiB: 1024}, {pid: 14, tree: true},
		{pid: 16, tree: true, needMiB: 1024}, {pid: 16, tree: true, needMiB: 1024}})
	if !reflect.DeepEqual(roots, map[int]int64{10: 0}) || !reflect.DeepEqual(stop, map[int]bool{10: true, 12: true, 14: true, 16: true}) {
		t.Errorf("use asked roots %v, stop %v; want roots map[10:0], stop map[10 12 14 16]", roots, stop)
	}
}
