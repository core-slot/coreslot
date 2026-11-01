//go:build unix

package main

import (
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/store"
)

// peak is the largest reading of the command tree's memory and when it was taken, after the
// command's start; ok is false when no reading succeeded.
type peak struct {
	kib    int64
	afterS float64
	ok     bool
}

// setMemPeak records the larger of the sampled peak and the rusage's maxRSSKiB (nil when unknown)
// as the run's memory peak. The rusage is a floor: it sees one process at a time, never a tree's
// sum.
func setMemPeak(rec *store.RunRecord, maxRSSKiB *int64, p peak) {
	switch {
	case p.ok && (maxRSSKiB == nil || p.kib >= *maxRSSKiB):
		rec.MemPeakKiB, rec.MemPeakSource, rec.PeakAfterS = &p.kib, memsample.Source, &p.afterS
	case maxRSSKiB != nil:
		rec.MemPeakKiB, rec.MemPeakSource = maxRSSKiB, "max_rss"
	}
}
