package memsample

import (
	"slices"
	"testing"
)

// rank returns the n largest by footprint, biggest first. Mutations: sorting ascending, or not
// capping at n. (Which process is the VM no longer depends on a name: see dockervm_test.go.)
func TestSweepTopIsSortedAndCapped(t *testing.T) {
	procs := []ProcMem{{Name: "a", FootprintMiB: 5}, {Name: "b", FootprintMiB: 50},
		{Name: "c", FootprintMiB: 500}, {Name: "d", FootprintMiB: 20}}
	top := rank(slices.Clone(procs), 3)
	var names []string
	for _, p := range top {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"c", "b", "d"}) {
		t.Errorf("top = %v, want [c b d]", names)
	}
	if all := rank(slices.Clone(procs), 99); len(all) != len(procs) {
		t.Errorf("n above the count gave %d, want %d", len(all), len(procs))
	}
	if none := rank(slices.Clone(procs), 0); len(none) != 0 {
		t.Errorf("n of 0 gave %d", len(none))
	}
}
