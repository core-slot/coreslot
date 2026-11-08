package memsample

import (
	"cmp"
	"slices"
	"strings"
)

// ProcMem is what one process holds: its name, its physical footprint and resident size in MiB, the
// CPU time it has spent in seconds, and its start time in ns since the epoch.
type ProcMem struct {
	Name                      string
	FootprintMiB, ResidentMiB int64
	CPUS                      float64
	StartNS                   int64
}

// vmNames are the names, by prefix, of the processes that hold the Docker VM's memory. The kernel
// keeps 31 bytes of a name, so only a prefix is compared.
var vmNames = []string{"com.docker.krun", "com.docker.sailor", "com.docker.virtualization", "com.apple.Virtualization"}

// isVM reports whether name is one of the Docker VM's process names.
func isVM(name string) bool {
	return slices.ContainsFunc(vmNames, func(p string) bool { return strings.HasPrefix(name, p) })
}

// pickVM returns the VM process with the largest footprint among procs, or nil: the small helper
// of the VM runs beside it and must not be taken for it.
func pickVM(procs []ProcMem) *ProcMem {
	var vm *ProcMem
	for i := range procs {
		if isVM(procs[i].Name) && (vm == nil || procs[i].FootprintMiB > vm.FootprintMiB) {
			vm = &procs[i]
		}
	}
	if vm == nil {
		return nil
	}
	c := *vm
	return &c
}

// rank returns the n largest of procs by footprint and the VM among all of them.
func rank(procs []ProcMem, n int) (top []ProcMem, vm *ProcMem) {
	vm = pickVM(procs)
	slices.SortStableFunc(procs, func(a, b ProcMem) int { return cmp.Compare(b.FootprintMiB, a.FootprintMiB) })
	return procs[:min(max(n, 0), len(procs))], vm
}
