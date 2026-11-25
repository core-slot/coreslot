//go:build unix

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
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

// ticks feeds readings to a fresh peak's tick, a negative reading as a listing that failed.
func ticks(readings ...int64) peak {
	var p peak
	for _, r := range readings {
		p.tick(max(r, 0), r >= 0)
	}
	return p
}

// The sustained level is the largest, over every sustainTicks tick readings in a row, of their
// least. Mutations: sustainTicks − 1, or report the least reading so far → a level after four
// readings; the largest of each row → 9 for the dip; keep the first row → 1 and 4.
func TestSustainedRows(t *testing.T) {
	if p := ticks(5, 5, 5, 5); p.sustained {
		t.Errorf("four readings: sustained %d; want none", p.sustainedKiB)
	}
	for _, c := range []struct {
		readings []int64
		want     int64
	}{
		{[]int64{5, 5, 5, 5, 5}, 5},
		{[]int64{1, 9, 9, 9, 9, 9, 2}, 9},
		{[]int64{9, 9, 3, 9, 9, 9, 9}, 3},
		{[]int64{4, 4, 4, 4, 4, 8, 8, 8, 8, 8}, 8},
	} {
		if p := ticks(c.readings...); !p.sustained || p.sustainedKiB != c.want {
			t.Errorf("%v: sustained %v %d; want %d", c.readings, p.sustained, p.sustainedKiB, c.want)
		}
	}
}

// A tick whose listing failed ends the row: four readings on each side of it are no level. Mutation:
// skip a failed tick instead of ending the row → 7.
func TestSustainedFailedTickEndsRow(t *testing.T) {
	readings := []int64{7, 7, 7, 7, -1, 7, 7, 7, 7}
	if p := ticks(readings...); p.sustained {
		t.Errorf("%v: sustained %d; want none", readings, p.sustainedKiB)
	}
	if p := ticks(append(readings, 7)...); !p.sustained || p.sustainedKiB != 7 {
		t.Errorf("one more 7: sustained %v %d; want 7", p.sustained, p.sustainedKiB)
	}
}

// The record carries the sustained level beside the peak, the rusage floor raising the peak alone,
// and no key without a level. Mutation: write the key whenever p.ok → the last record has it.
func TestSetMemPeakSustained(t *testing.T) {
	p := peak{kib: 9, ok: true, sustainedKiB: 5, sustained: true}
	for _, c := range []struct {
		rss  *int64
		peak int64
	}{{nil, 9}, {new(int64(12)), 12}} {
		var rec store.RunRecord
		setMemPeak(&rec, c.rss, p)
		if rec.MemPeakKiB == nil || *rec.MemPeakKiB != c.peak || rec.MemSustainedKiB == nil || *rec.MemSustainedKiB != 5 {
			t.Errorf("rss %v: peak %v sustained %v; want %d and 5", c.rss, rec.MemPeakKiB, rec.MemSustainedKiB, c.peak)
		}
	}
	p.sustained = false
	var rec store.RunRecord
	setMemPeak(&rec, nil, p)
	if rec.MemSustainedKiB != nil {
		t.Errorf("no level: sustained %d; want none", *rec.MemSustainedKiB)
	}
}

// An attempt with a grant records in its run-log line the window its readings needed, from the
// listings that succeeded: 100, 100, 1500, 1500, 3000 with a margin of 400 need 5 readings, 5 s.
// A stopped attempt's line carries one too; an attempt without a grant carries none. Mutations:
// appending a failed listing's 0 too gives 7 s; returning a stopped attempt's peak without its
// readings leaves its line without the key; keeping readings without a grant gives the third a key.
func TestRunLogCarriesMemWindow(t *testing.T) {
	windowOf := func(p peak) float64 { // 0 when the line has no key
		var rec store.RunRecord
		if setMemPeak(&rec, nil, p); rec.MemWindowS == nil {
			return 0
		}
		return *rec.MemWindowS
	}
	_, g, st := trimHold(t, 4000)
	_, p := trimWatch(t, trimRun(t, 60), g, st, []int64{100, 100, 1500, -1, -1, 1500, 3000}, nil)
	if w := windowOf(p); w != 5 {
		t.Errorf("mem_window_s %v; want 5", w)
	}
	g, st = watchedHold(t)
	storeRound(t, st, watch.Detector{Level: watch.Red, Stop: []string{g.HoldName()}, LastRoundNS: 1})
	countKills(t)
	stopped, p := watchFor(t, restartable("make", "test"), &supervisor{pid: 42, group: true}, g, st, 6, func(int) bool { return false }, nil)
	if w := windowOf(p); !stopped || w == 0 {
		t.Errorf("stopped %v, mem_window_s %v; want a stopped attempt with the key", stopped, w)
	}
	_, p = watchFor(t, restartable("make", "test"), &supervisor{pid: 42}, nil, nil, 3, func(int) bool { return false }, nil)
	if w := windowOf(p); w != 0 {
		t.Errorf("without a grant: mem_window_s %v; want none", w)
	}
}
