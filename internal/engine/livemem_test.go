//go:build unix

package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// Every engine test measures no tree unless it sets one, so that no admission depends on the
// test machine's processes. Helper processes run this init too.
func init() {
	liveMem.use = func(map[int]int64, map[int]bool) map[int]int64 { return nil }
}

// setLiveMem replaces the headroom testMachine reports, in MiB, and the use in KiB that every
// root asked about reports (none when useKiB is negative), for one test.
func setLiveMem(t *testing.T, headroomMiB, useKiB int64) {
	t.Helper()
	savedUse, savedHeadroom := liveMem, testHeadroom
	t.Cleanup(func() { liveMem, testHeadroom = savedUse, savedHeadroom })
	testHeadroom.mib, testHeadroom.ok = headroomMiB, true
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
}

// The room is the headroom less the reserve and every live hold's unmeasured need: of 4096 MiB,
// 1024 are the reserve and A's 1 GiB, so B's 2 GiB fits exactly; C's does not. Mutation: forgetting
// the reserve admits C; comparing need < room refuses B.
func TestRoomGatesAdmission(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	setLiveMem(t, 4096, -1)
	admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	admitted(t, step(t, enqueue(t, e, need("mem=2G"))))
	if r := step(t, enqueue(t, e, need("mem=1M"))); isAdmitted(r) {
		t.Fatal("a request admitted with no room left")
	}
}

// With no hold live the head starts whatever the numbers say; the next request, with a hold live,
// is held to them. Mutation: gating on the room when idle refuses the first.
func TestIdleHeadAlwaysStarts(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	setLiveMem(t, 100, -1) // below the reserve
	g := admitted(t, step(t, enqueue(t, e, need("mem=4G"))))
	if g.GrantMiB() != 4096 {
		t.Errorf("grant %d, want the 4096 asked", g.GrantMiB())
	}
	if r := step(t, enqueue(t, e, need("mem=1M"))); isAdmitted(r) {
		t.Fatal("a request admitted beside a live hold with the room negative")
	}
}

// A headroom that cannot be read gates nothing, without an error, and lets nobody pass a ticket
// ahead. Mutation: reading the headroom as 0 when it is not ok, and B waits.
func TestUnreadHeadroomGatesNothing(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	setLiveMem(t, 0, -1)
	testHeadroom.ok = false
	admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	admitted(t, step(t, enqueue(t, e, need("mem=2G"))))
}

// A hold hands the room back by what its tree already uses: with 3584 MiB, A's 1 GiB unmeasured
// leaves 1536 MiB after the reserve, too little for B's 2 GiB. With A's tree measured at 1 GiB
// nothing of it is still to grow, 2560 MiB are free and B is admitted. Mutation: measure taking
// every hold as claiming its tree reddens the first half; never asking use reddens the second.
func TestRoomUnmeasuredHoldCountsWhole(t *testing.T) {
	for _, c := range []struct {
		name  string
		tree  bool
		admit bool
	}{{"no tree", false, false}, {"tree measured", true, true}} {
		t.Run(c.name, func(t *testing.T) {
			e := newEngine(t, newStateDir(t, ""), newFakeClock())
			setLiveMem(t, 3584, 1<<20)
			req := need("mem=1G")
			req.Tree = c.tree
			admitted(t, step(t, enqueue(t, e, req)))
			if r := step(t, enqueue(t, e, need("mem=2G"))); isAdmitted(r) != c.admit {
				t.Errorf("B admitted %v, want %v", isAdmitted(r), c.admit)
			}
		})
	}
}

// A live hold whose body does not parse (its run rewrites it in place, so a read can be torn)
// counts at the cold start of the machine, 426 MiB of 1707, not at zero: 2048 MiB of headroom less
// the 1024 reserve and 426 leaves 598. Mutation: counting it at zero admits 600.
func TestTornLiveHoldCountsAtColdStart(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 2048, -1)
	if err := os.MkdirAll(filepath.Join(dir, store.HoldsDir), 0o700); err != nil {
		t.Fatal(err)
	}
	torn := filepath.Join(dir, store.HoldsDir, wire.HoldName(1, 4242, 1, "torn"))
	if err := os.WriteFile(torn, []byte(`{"label":"torn","pid":42`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(torn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	over := enqueue(t, e, need("mem=600M"))
	if r := step(t, over); isAdmitted(r) {
		t.Fatal("600 MiB admitted with the torn hold counted at zero")
	}
	if err := over.Withdraw(); err != nil {
		t.Fatal(err)
	}
	admitted(t, step(t, enqueue(t, e, need("mem=598M"))))
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
