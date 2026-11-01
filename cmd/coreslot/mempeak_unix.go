//go:build unix

package main

import "github.com/core-slot/coreslot/internal/store"

// peak is the largest reading of the command tree's memory; ok is false when no reading succeeded.
type peak struct {
	kib int64
	ok  bool
}

// setMemPeak records the larger of the sampled peak and the rusage's maxRSSKiB (nil when unknown)
// as the run's memory peak. The rusage is a floor: it sees one process at a time, never a tree's
// sum.
func setMemPeak(rec *store.RunRecord, maxRSSKiB *int64, p peak) {
	switch {
	case p.ok && (maxRSSKiB == nil || p.kib >= *maxRSSKiB):
		rec.MemPeakKiB = &p.kib
	case maxRSSKiB != nil:
		rec.MemPeakKiB = maxRSSKiB
	}
}
