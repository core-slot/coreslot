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
	listTree = func(pid int) (int64, map[int]memsample.TreeTimes, bool) {
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
		_, p := (&run{}).watchChild(ctx, &supervisor{pid: 42}, nil, nil)
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
}

func TestSetMemPeakFloorWins(t *testing.T) {
	rss := int64(500)
	var rec store.RunRecord
	setMemPeak(&rec, &rss, peak{kib: 100, ok: true})
	if rec.MemPeakKiB == nil || *rec.MemPeakKiB != 500 {
		t.Errorf("mem peak %v, want 500", rec.MemPeakKiB)
	}
}

func TestSetMemPeakSampledWins(t *testing.T) {
	rss := int64(500)
	var rec store.RunRecord
	setMemPeak(&rec, &rss, peak{kib: 900, ok: true})
	if rec.MemPeakKiB == nil || *rec.MemPeakKiB != 900 {
		t.Errorf("mem peak %v, want 900", rec.MemPeakKiB)
	}
}

// Mutation: record a zero peak when nothing was measured.
func TestSetMemPeakUnknown(t *testing.T) {
	var rec store.RunRecord
	setMemPeak(&rec, nil, peak{})
	if rec.MemPeakKiB != nil {
		t.Errorf("mem peak %v, want none", rec.MemPeakKiB)
	}
}
