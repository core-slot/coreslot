package memsample

import (
	"fmt"
	"testing"
	"time"
)

func TestMeminfo(t *testing.T) {
	const text = "MemTotal:       16303412 kB\nMemAvailable: 9000000 kB\n"
	for key, want := range map[string]int64{"MemTotal": 16303412, "MemAvailable": 9000000} {
		if got, ok := meminfo(text, key); !ok || got != want {
			t.Errorf("meminfo(%s) = %d, %v; want %d", key, got, ok, want)
		}
	}
	if got, ok := meminfo(text, "Foo"); ok {
		t.Errorf("meminfo(Foo) = %d, true; want not found", got)
	}
}

// Mutation: stop the walk at the root's children (no grandchildren), and 6 is missing.
func TestDescendants(t *testing.T) {
	parents := map[int]int{2: 1, 3: 2, 4: 2, 5: 1, 6: 3}
	if got := fmt.Sprint(descendants(2, parents, nil)); got != "[2 3 4 6]" {
		t.Errorf("descendants(2) = %s, want [2 3 4 6]", got)
	}
}

// A process that exited after the listing cannot be read: it is skipped, and the rest still
// add up. Mutation: return !ok on the first failed read.
func TestTreeSumSkipsFailedReads(t *testing.T) {
	read := func(pid int) (int64, bool) {
		switch pid {
		case 2:
			return 100, true
		case 4:
			return 50, true
		}
		return 0, false
	}
	if kib, ok := treeSum(2, map[int]int{3: 2, 4: 2}, read); kib != 150 || !ok {
		t.Errorf("treeSum = %d, %v; want 150, true", kib, ok)
	}
}

// A root that is gone and lists no descendants reads nothing: not a measured zero. Mutation:
// return true for an empty sum.
func TestTreeSumRootGone(t *testing.T) {
	read := func(int) (int64, bool) { return 0, false }
	if kib, ok := treeSum(7, map[int]int{3: 2}, read); kib != 0 || ok {
		t.Errorf("treeSum = %d, %v; want 0, false", kib, ok)
	}
}

// residentOf reads private and shared KiB from a map; a pid not in it cannot be read.
func residentOf(m map[int][2]int64) func(int) (int64, int64, bool) {
	return func(pid int) (int64, int64, bool) {
		r, ok := m[pid]
		return r[0], r[1], ok
	}
}

// A hold nested inside another hold's tree is that hold's own: the outer one leaves its subtree
// out. Mutation: walk descendants without the stop at other roots, and 10's use is 3110.
func TestHolderUseExcludesNestedRoot(t *testing.T) {
	procs := map[int]Proc{10: {PPID: 1}, 11: {PPID: 10}, 12: {PPID: 11}, 13: {PPID: 12}}
	read := residentOf(map[int][2]int64{10: {100, 0}, 11: {10, 0}, 12: {1000, 0}, 13: {2000, 0}})
	got := holderUse(procs, map[int]int64{10: 0, 12: 0}, read)
	if got[10] != 110 || got[12] != 3000 || len(got) != 2 {
		t.Errorf("holderUse = %v; want map[10:110 12:3000]", got)
	}
}

// A pid that started 2 s after its hold began is not the holder: the holder died and the pid was
// reused. Mutation: drop the start check, and 10 is present.
func TestHolderUseReusedPID(t *testing.T) {
	const start = int64(1_000 * time.Second)
	procs := map[int]Proc{
		10: {PPID: 1, StartNS: start + int64(2*time.Second)},
		20: {PPID: 1, StartNS: start - int64(time.Second)},
	}
	read := residentOf(map[int][2]int64{10: {100, 0}, 20: {200, 0}})
	got := holderUse(procs, map[int]int64{10: start, 20: start}, read)
	if _, ok := got[10]; ok || got[20] != 200 {
		t.Errorf("holderUse = %v; want map[20:200]", got)
	}
}

// Shared memory is mapped by several processes of the tree but held once: the largest counts.
// Mutation: add every process's shared, and the use is 950.
func TestHolderUseSharedOnce(t *testing.T) {
	procs := map[int]Proc{10: {PPID: 1}, 11: {PPID: 10}}
	read := residentOf(map[int][2]int64{10: {100, 300}, 11: {50, 500}})
	if got := holderUse(procs, map[int]int64{10: 0}, read); got[10] != 650 {
		t.Errorf("holderUse = %v; want map[10:650]", got)
	}
}

// A tree none of whose processes reads is not a measured zero. Mutation: record the sum whether
// or not anything was read, and 10 is present with 0.
func TestHolderUseUnreadable(t *testing.T) {
	procs := map[int]Proc{10: {PPID: 1}, 11: {PPID: 10}}
	if got := holderUse(procs, map[int]int64{10: 0}, residentOf(nil)); len(got) != 0 {
		t.Errorf("holderUse = %v; want empty", got)
	}
}

// A root missing from the listing has died; a child still listed under it is not its use.
// Mutation: drop the check that the root is in procs, and 10 is present with 40.
func TestHolderUseRootGone(t *testing.T) {
	procs := map[int]Proc{11: {PPID: 10}}
	read := residentOf(map[int][2]int64{11: {40, 0}})
	if got := holderUse(procs, map[int]int64{10: 0}, read); len(got) != 0 {
		t.Errorf("holderUse = %v; want empty", got)
	}
}
