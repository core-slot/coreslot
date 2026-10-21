//go:build unix

package engine

import (
	"context"
	"reflect"
	"testing"

	"github.com/core-slot/coreslot/internal/spec"
)

// Every engine test reads no headroom unless it sets one, so that no mem admission, wait or
// clamp depends on the test machine's memory. Helper processes run this init too.
func init() {
	liveMem.headroom = func() (int64, bool) { return 0, false }
	liveMem.use = func(map[int]int64) map[int]int64 { return nil }
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
	liveMem.use = func(roots map[int]int64) map[int]int64 {
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

const liveCfg = "[resources]\nmem = \"1G\"\n" // 8 mem tokens

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
	dir := newStateDir(t, liveCfg+"[memory]\nlive = false\n")
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 4096, -1)
	admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	if r := step(t, enqueue(t, e, need("mem=2G"))); isAdmitted(r) {
		t.Fatal("mem=2G admitted with live = false and the static size held")
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
