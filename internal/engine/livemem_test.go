//go:build unix

package engine

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
)

// Every engine test reads no headroom unless it sets one, so that no mem admission, wait or
// clamp depends on the test machine's memory. Helper processes run this init too.
func init() {
	liveMem.headroom = func() (int64, bool) { return 0, false }
	liveMem.use = func(map[int]int64, map[int]bool) map[int]int64 { return nil }
}

// setLiveMem replaces the live size's readings for one test: the headroom in MiB, and the use in
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

const staticCfg = "[resources]\nmem = \"1G\"\n"       // 8 mem tokens
const liveCfg = staticCfg + "[memory]\nlive = true\n" // the live size is off unless turned on

// treeNeed is a request for spec whose hold claims this process's tree.
func treeNeed(spec string) spec.Request {
	r := need(spec)
	r.Tree = true
	return r
}

// A fixed mem need above what the static size leaves free is admitted from the headroom: 4096
// MiB less A's unmeasured 1 GiB is 24 tokens on top of A's 8. Mutation: attemptSizes returning
// sizes unchanged, and B waits.
func TestLiveAdmitsAboveStatic(t *testing.T) {
	dir := newStateDir(t, liveCfg)
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 4096, -1)
	admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	g := admitted(t, step(t, enqueue(t, e, need("mem=2G"))))
	if g.Grant()["mem"] != 16 || !g.LiveMem() || len(g.Clamped()) != 0 {
		t.Errorf("grant %v, LiveMem %v, clamped %v; want mem 16 from a live size, not clamped",
			g.Grant(), g.LiveMem(), g.Clamped())
	}
}

// A need the static size covers never reads the headroom. Mutation: computing the live size
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

// [memory] live = false keeps the static size. Mutation: attemptSizes ignoring LiveMem.
func TestLiveOffByConfig(t *testing.T) {
	dir := newStateDir(t, staticCfg+"[memory]\nlive = false\n")
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 4096, -1)
	admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	if r := step(t, enqueue(t, e, need("mem=2G"))); isAdmitted(r) {
		t.Fatal("mem=2G admitted with live = false and the static size held")
	}
}

// A waiter whose fixed mem need only the live size would admit does not pass a live ticket ahead
// that is short of mem on the configured size: H waits for 4 of the 2 tokens A leaves, and P's 3
// would come from the headroom, taking what H waits for. Withdrawn, H no longer holds P back.
// Mutation: drop the live test from the passer's checks in admit, and P is admitted past H.
func TestLivePasserDoesNotPass(t *testing.T) {
	dir := newStateDir(t, liveCfg)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	setLiveMem(t, 4096, -1)
	admitted(t, step(t, enqueue(t, e, need("mem=768M"))))
	h := enqueue(t, e, spec.Request{Need: map[string]spec.Range{"mem": {Min: 4, Max: 8}}})
	p := enqueue(t, e, need("mem=384M"))
	if r := step(t, h); isAdmitted(r) {
		t.Fatal("H admitted with 2 mem tokens free")
	}
	if r := step(t, p); isAdmitted(r) {
		t.Fatal("P admitted past H from the live size")
	}
	if err := h.Withdraw(); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second)
	if g := admitted(t, step(t, p)); !g.LiveMem() {
		t.Errorf("P, alone, granted %v without the live size", g.Grant())
	}
}

// A mem range is sized statically. Mutation: attemptSizes not requiring Min == Max.
func TestLiveRangeRequestIsStatic(t *testing.T) {
	dir := newStateDir(t, liveCfg)
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 4096, -1)
	admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	req := spec.Request{Need: map[string]spec.Range{"mem": {Min: 16, Max: 32}}}
	if r := step(t, enqueue(t, e, req)); isAdmitted(r) {
		t.Fatal("mem 16..32 admitted from a live size")
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

// A hold without tree counts its whole reservation: 2560 MiB less A's 1 GiB is 12 tokens, too
// few for B's 16. With A's tree measured at 1 GiB nothing of it is unrealised, 20 tokens are
// free and B is admitted. Mutation: liveMemHolds taking every hold as claiming its tree reddens
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

// A fixed need above the live size is clamped to it: 3072 MiB free is 24 tokens, not the static
// 8. Mutation: Effective not run again over the raised sizes, and the clamp is to 8.
func TestLiveClampAgainstLiveSize(t *testing.T) {
	dir := newStateDir(t, liveCfg)
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 3072, -1)
	g := admitted(t, step(t, enqueue(t, e, need("mem=4G"))))
	if g.Grant()["mem"] != 24 || !reflect.DeepEqual(g.Clamped(), map[string]int{"mem": 32}) || !g.LiveMem() {
		t.Errorf("grant %v, clamped %v, LiveMem %v; want mem 24, clamped mem 32, true",
			g.Grant(), g.Clamped(), g.LiveMem())
	}
}

// An adoption sizes mem from the headroom as an admission does. Mutation: calling attemptSizes
// only from admit, and the adoption takes 0.
func TestAdoptLive(t *testing.T) {
	dir := newStateDir(t, liveCfg)
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 4096, -1)
	admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	g, err := e.AdoptContext(context.Background(), need("mem=2G"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.Grant()["mem"] != 16 || !g.LiveMem() {
		t.Errorf("adopted %v, LiveMem %v; want mem 16 and true", g.Grant(), g.LiveMem())
	}
}

// An adopted hold carries no tree, so its whole reservation counts even when its pid's tree is
// measured: 2560 MiB less 1 GiB leaves 12 tokens, too few for 16. Mutation: publishHold writing
// the request's tree for an adoption too, and B is admitted.
func TestLiveAdoptedHoldCountsWhole(t *testing.T) {
	dir := newStateDir(t, liveCfg)
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 2560, 1<<20)
	g, err := e.AdoptContext(context.Background(), treeNeed("mem=1G"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if got := readHold(t, e, g); got.Tree || len(got.Grant["mem"]) != 8 {
		t.Fatalf("adopted hold %+v, want 8 mem tokens and no tree", got)
	}
	if r := step(t, enqueue(t, e, need("mem=2G"))); isAdmitted(r) {
		t.Fatal("mem=2G admitted: the adopted hold's reservation was measured")
	}
}

// B's process runs inside A's tree and B's whole reservation counts, so A's tree is read without
// B's subtree; C, a tree hold with no mem tokens, stops the walk unread; D and E name one pid and
// both count whole. Mutation: stop only at the measured roots, and 12 is no stop; roots without
// the h.tokens > 0 check, and 14 is a root; claims[h.pid] == 1 as >= 1, and 16 is a root.
func TestMeasureStopsAtEveryLiveHold(t *testing.T) {
	setLiveMem(t, 0, -1)
	var roots map[int]int64
	var stop map[int]bool
	liveMem.use = func(r map[int]int64, s map[int]bool) map[int]int64 {
		roots, stop = r, s
		return nil
	}
	measure([]memHold{{pid: 10, tree: true, tokens: 32}, {pid: 12, tokens: 8}, {pid: 14, tree: true},
		{pid: 16, tree: true, tokens: 8}, {pid: 16, tree: true, tokens: 8}})
	if !reflect.DeepEqual(roots, map[int]int64{10: 0}) || !reflect.DeepEqual(stop, map[int]bool{10: true, 12: true, 14: true, 16: true}) {
		t.Errorf("use asked roots %v, stop %v; want roots map[10:0], stop map[10 12 14 16]", roots, stop)
	}
}
