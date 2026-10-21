//go:build unix

package main

import (
	"time"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/store"
)

// peakSampler reads the memory of a command's process tree on a period and keeps the largest
// reading, with when it was taken.
type peakSampler struct {
	done   chan struct{}
	result chan peak
}

type peak struct {
	kib    int64
	afterS float64
	ok     bool
}

// startPeakSampler reads tree(pid) every period, the first time one period after the start.
func startPeakSampler(pid int, every time.Duration, tree func(int) (int64, bool)) *peakSampler {
	p := &peakSampler{done: make(chan struct{}), result: make(chan peak, 1)}
	start := time.Now()
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		var best peak
		for {
			select {
			case <-p.done:
				p.result <- best
				return
			case <-t.C:
				if kib, ok := tree(pid); ok && (!best.ok || kib > best.kib) {
					best = peak{kib: kib, afterS: time.Since(start).Seconds(), ok: true}
				}
			}
		}
	}()
	return p
}

// stop ends the sampling and returns the peak; ok is false when no reading succeeded.
func (p *peakSampler) stop() (kib int64, afterS float64, ok bool) {
	close(p.done)
	b := <-p.result
	return b.kib, b.afterS, b.ok
}

// setMemPeak records the larger of the sampled peak and the rusage's MaxRSSKiB as the run's
// memory peak. The rusage is a floor: it sees one process at a time, never a tree's sum.
func setMemPeak(rec *store.RunRecord, kib int64, afterS float64, sampled bool) {
	switch {
	case sampled && (rec.MaxRSSKiB == nil || kib >= *rec.MaxRSSKiB):
		rec.MemPeakKiB, rec.MemPeakSource, rec.PeakAfterS = &kib, memsample.Source, &afterS
	case rec.MaxRSSKiB != nil:
		rss := *rec.MaxRSSKiB
		rec.MemPeakKiB, rec.MemPeakSource = &rss, "max_rss"
	}
}
