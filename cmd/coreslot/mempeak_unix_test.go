//go:build unix

package main

import (
	"sync"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/store"
)

// The sampler keeps the largest reading and when it was taken, not the last one. Mutation: keep
// the last sample instead of the max, and the peak is 200.
func TestPeakSamplerKeepsMax(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	tree := func(pid int) (int64, bool) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		switch calls {
		case 1:
			return 100, true
		case 2:
			return 300, true
		}
		return 200, true
	}
	p := startPeakSampler(42, 10*time.Millisecond, tree)
	waitFor(t, 10*time.Second, "four samples", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 4
	})
	kib, afterS, ok := p.stop()
	if !ok || kib != 300 {
		t.Fatalf("stop = %d, %v, %v; want 300 KiB", kib, afterS, ok)
	}
	if afterS < 0.02 || afterS > 1 {
		t.Errorf("peak after %v s, want about 0.02 s: the second tick", afterS)
	}
}

func TestSetMemPeakFloorWins(t *testing.T) {
	rss := int64(500)
	rec := store.RunRecord{MaxRSSKiB: &rss}
	setMemPeak(&rec, 100, 1.5, true)
	if rec.MemPeakKiB == nil || *rec.MemPeakKiB != 500 || rec.MemPeakSource != "max_rss" || rec.PeakAfterS != nil {
		t.Errorf("mem peak %v, %q, %v; want 500, max_rss, no time", rec.MemPeakKiB, rec.MemPeakSource, rec.PeakAfterS)
	}
}

func TestSetMemPeakSampledWins(t *testing.T) {
	rss := int64(500)
	rec := store.RunRecord{MaxRSSKiB: &rss}
	setMemPeak(&rec, 900, 2.5, true)
	if rec.MemPeakKiB == nil || *rec.MemPeakKiB != 900 || rec.MemPeakSource != memsample.Source ||
		rec.PeakAfterS == nil || *rec.PeakAfterS != 2.5 {
		t.Errorf("mem peak %v, %q, %v; want 900, %q, 2.5", rec.MemPeakKiB, rec.MemPeakSource, rec.PeakAfterS, memsample.Source)
	}
}

// Mutation: record a zero peak when nothing was measured.
func TestSetMemPeakUnknown(t *testing.T) {
	var rec store.RunRecord
	setMemPeak(&rec, 0, 0, false)
	if rec.MemPeakKiB != nil || rec.MemPeakSource != "" || rec.PeakAfterS != nil {
		t.Errorf("mem peak %v, %q, %v; want none", rec.MemPeakKiB, rec.MemPeakSource, rec.PeakAfterS)
	}
}
