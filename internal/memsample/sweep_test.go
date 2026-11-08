package memsample

import (
	"slices"
	"testing"
)

// The VM is the largest footprint among the matches, wherever it stands in the input. Mutation:
// taking the first match returns the 40 MiB helper that comes first.
func TestVMPicksLargest(t *testing.T) {
	procs := []ProcMem{{Name: "Safari", FootprintMiB: 9000}, {Name: "com.docker.krun", FootprintMiB: 40},
		{Name: "com.docker.virtualization", FootprintMiB: 6000, StartNS: 7}, {Name: "com.docker.virtualization", FootprintMiB: 5}}
	if vm := pickVM(procs); vm == nil || vm.FootprintMiB != 6000 || vm.StartNS != 7 {
		t.Errorf("pickVM = %+v, want the 6000 MiB virtualization", vm)
	}
	if pickVM([]ProcMem{{Name: "Safari", FootprintMiB: 9000}}) != nil {
		t.Error("pickVM found a VM among unrelated processes")
	}
	if pickVM(nil) != nil {
		t.Error("pickVM found a VM in nothing")
	}
}

// The kernel keeps 31 bytes of a name, so a VM process's name is matched by prefix. Mutation: an
// exact match of the full names leaves every truncated form unmatched.
func TestVMNameMatchesTruncated(t *testing.T) {
	for _, full := range []string{"com.docker.krun", "com.docker.virtualization", "com.apple.Virtualization.VirtualMachine",
		"com.docker.virtualization.something-long-here"} {
		name := full[:min(len(full), 31)]
		if !isVM(name) {
			t.Errorf("isVM(%q) is false", name)
		}
	}
	for _, name := range []string{"", "com.docker", "docker", "com.apple.Virtual", "Docker Desktop"} {
		if isVM(name) {
			t.Errorf("isVM(%q) is true", name)
		}
	}
}

// rank returns the n largest by footprint, biggest first, and finds the VM outside the cut.
// Mutations: sorting ascending, or looking for the VM only in the cut.
func TestSweepTopIsSortedAndCapped(t *testing.T) {
	procs := []ProcMem{{Name: "a", FootprintMiB: 5}, {Name: "b", FootprintMiB: 50}, {Name: "com.apple.Virtualization.Virtua", FootprintMiB: 1},
		{Name: "c", FootprintMiB: 500}, {Name: "d", FootprintMiB: 20}}
	top, vm := rank(slices.Clone(procs), 3)
	var names []string
	for _, p := range top {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"c", "b", "d"}) {
		t.Errorf("top = %v, want [c b d]", names)
	}
	if vm == nil || vm.FootprintMiB != 1 {
		t.Errorf("vm = %+v, want the 1 MiB one outside the top", vm)
	}
	if all, _ := rank(slices.Clone(procs), 99); len(all) != len(procs) {
		t.Errorf("n above the count gave %d, want %d", len(all), len(procs))
	}
	if none, _ := rank(slices.Clone(procs), 0); len(none) != 0 {
		t.Errorf("n of 0 gave %d", len(none))
	}
}
