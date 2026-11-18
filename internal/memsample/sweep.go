package memsample

import (
	"cmp"
	"slices"
)

// ProcMem is what one process holds: its name, its physical footprint and resident size in MiB, the
// CPU time it has spent in seconds, and its start time in ns since the epoch.
type ProcMem struct {
	Name                      string
	FootprintMiB, ResidentMiB int64
	CPUS                      float64
	StartNS                   int64
}

// rank returns the n largest of procs by footprint.
func rank(procs []ProcMem, n int) []ProcMem {
	slices.SortStableFunc(procs, func(a, b ProcMem) int { return cmp.Compare(b.FootprintMiB, a.FootprintMiB) })
	return procs[:min(max(n, 0), len(procs))]
}
