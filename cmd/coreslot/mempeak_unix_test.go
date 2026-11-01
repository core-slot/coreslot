//go:build unix

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/store"
)

// The tree's listing keeps the largest reading and when it was taken, not the last one.
// Mutation: keep the last sample instead of the max, and the peak is 200.
func TestPeakSamplerKeepsMax(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	saved := listTree
	t.Cleanup(func() { listTree = saved })
	listTree = func(pid int) (int64, []int, bool) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		switch calls {
		case 1:
			return 100, nil, true
		case 2:
			return 300, nil, true
		}
		return 200, nil, true
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan peak)
	go func() {
		_, p := (&run{}).watchChild(ctx, &supervisor{pid: 42}, nil)
		done <- p
	}()
	waitFor(t, 10*time.Second, "four samples", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 4
	})
	cancel()
	p := <-done
	if !p.ok || p.kib != 300 {
		t.Fatalf("peak %+v; want 300 KiB", p)
	}
	if p.afterS < 1.5 || p.afterS > 3 {
		t.Errorf("peak after %v s, want about 2 s: the second listing", p.afterS)
	}
}

func TestSetMemPeakFloorWins(t *testing.T) {
	rss := int64(500)
	var rec store.RunRecord
	setMemPeak(&rec, &rss, peak{kib: 100, afterS: 1.5, ok: true})
	if rec.MemPeakKiB == nil || *rec.MemPeakKiB != 500 || rec.MemPeakSource != "max_rss" || rec.PeakAfterS != nil {
		t.Errorf("mem peak %v, %q, %v; want 500, max_rss, no time", rec.MemPeakKiB, rec.MemPeakSource, rec.PeakAfterS)
	}
}

func TestSetMemPeakSampledWins(t *testing.T) {
	rss := int64(500)
	var rec store.RunRecord
	setMemPeak(&rec, &rss, peak{kib: 900, afterS: 2.5, ok: true})
	if rec.MemPeakKiB == nil || *rec.MemPeakKiB != 900 || rec.MemPeakSource != memsample.Source ||
		rec.PeakAfterS == nil || *rec.PeakAfterS != 2.5 {
		t.Errorf("mem peak %v, %q, %v; want 900, %q, 2.5", rec.MemPeakKiB, rec.MemPeakSource, rec.PeakAfterS, memsample.Source)
	}
}

// Mutation: record a zero peak when nothing was measured.
func TestSetMemPeakUnknown(t *testing.T) {
	var rec store.RunRecord
	setMemPeak(&rec, nil, peak{})
	if rec.MemPeakKiB != nil || rec.MemPeakSource != "" || rec.PeakAfterS != nil {
		t.Errorf("mem peak %v, %q, %v; want none", rec.MemPeakKiB, rec.MemPeakSource, rec.PeakAfterS)
	}
}
