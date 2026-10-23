package memsample

import (
	"fmt"
	"testing"
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
	if got := fmt.Sprint(descendants(2, parents)); got != "[2 3 4 6]" {
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
