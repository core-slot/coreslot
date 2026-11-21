//go:build unix

package main

import (
	"slices"

	"github.com/core-slot/coreslot/internal/store"
)

// sustainTicks is how many watch ticks in a row a level must hold to be the run's sustained level.
const sustainTicks = 5

// peak is the largest reading of the command tree's memory; ok is false when no reading succeeded.
// container says the run was container work: kib is its tree's, which leaves out its containers.
// sustainedKiB is the largest, over every sustainTicks tick readings in a row, of their least;
// sustained is false while no such row has been seen.
type peak struct {
	kib          int64
	ok           bool
	container    bool
	sumKiB       int64               // the readings that succeeded, summed
	samples      int64               // and counted, for the mean footprint
	row          [sustainTicks]int64 // the row's last readings, a ring
	rowN         int                 // the row's length so far
	sustainedKiB int64
	sustained    bool
}

// tick records one watch tick's reading of the tree; ok false, a listing that failed, ends the row.
func (p *peak) tick(kib int64, ok bool) {
	if !ok {
		p.rowN = 0
		return
	}
	p.row[p.rowN%sustainTicks] = kib
	p.rowN++
	if p.rowN < sustainTicks {
		return
	}
	if least := slices.Min(p.row[:]); !p.sustained || least > p.sustainedKiB {
		p.sustainedKiB, p.sustained = least, true
	}
}

// setMemPeak records the larger of the sampled peak and the rusage's maxRSSKiB (nil when unknown)
// as the run's memory peak, and the sustained level when the run had one. The rusage is a floor: it
// sees one process at a time, never a tree's sum.
func setMemPeak(rec *store.RunRecord, maxRSSKiB *int64, p peak) {
	switch {
	case p.ok && (maxRSSKiB == nil || p.kib >= *maxRSSKiB):
		rec.MemPeakKiB = &p.kib
	case maxRSSKiB != nil:
		rec.MemPeakKiB = maxRSSKiB
	}
	if p.sustained {
		rec.MemSustainedKiB = &p.sustainedKiB
	}
}
