//go:build unix

package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// listingsPerTick is how many of the tree's 1 s listings make one watch tick of the shared period.
const listingsPerTick = int(memsample.PeriodNS / int64(memSampleEvery))

// listTree is the listing of the command's tree; tests replace it.
var listTree = memsample.Tree

// watchChild lists the command's tree every second for its memory peak until ctx ends. Every
// second listing, when the run holds g, it writes the tree's footprint and time deltas into the
// hold and takes its part in the shared watch tick; when a stop round lists its hold, it kills
// the command's group and returns stopped, or in observe mode says what it would stop.
func (r *run) watchChild(ctx context.Context, sup *supervisor, g *engine.Grant, st *store.Store) (stopped bool, p peak) {
	t := time.NewTicker(memSampleEvery)
	defer t.Stop()
	prev := map[int]memsample.TreeTimes{}
	var round int64
	if g != nil { // constant for the attempt: written once, before its first tick
		r.updateHold(st, g, func(h *wire.Hold) {
			h.Victim, h.Stops, h.NormalShare, h.Learned = sup.group && !r.o.noRestart, r.stops, r.share, r.learned
		})
	}
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return false, p
		case <-t.C:
		}
		kib, times, ok := listTree(sup.pid)
		if ok && (!p.ok || kib > p.kib) {
			p = peak{kib: kib, ok: true}
		}
		if g == nil || n%listingsPerTick != 0 {
			continue
		}
		var d memsample.TreeTimes
		d, prev = treeDeltas(prev, times), times
		if !r.updateHold(st, g, func(h *wire.Hold) {
			h.FootprintMiB, h.DUserNS, h.DSysNS, h.DPageWaitNS = kib>>10, d.UserNS, d.SysNS, d.PageWaitNS
			h.SampledNS = store.NowNS()
		}) {
			continue
		}
		det, err := g.Tick(ctx)
		r.countClosed(d, det, err)
		if err != nil || det.LastRoundNS == round || !slices.Contains(det.Stop, g.HoldName()) {
			continue
		}
		round = det.LastRoundNS
		if r.stopFor(det, kib, d, sup.KillGroup) {
			return true, p
		}
	}
}

// stopFor acts on the stop round det last wrote, which lists this run, as that round was
// written, whatever the level or the mode is now: in observe mode it says what it would stop,
// otherwise it kills the command with kill and reports whether it did.
func (r *run) stopFor(det watch.Detector, kib int64, d memsample.TreeTimes, kill func() bool) bool {
	why := r.stopDetail(det.RoundFloor, kib, d)
	if det.Observed {
		r.notice("would stop %s (%s)", text(r.o.label), why)
		return false
	}
	if !kill() {
		return false
	}
	r.lastStop, r.lastFloor = why, det.RoundFloor
	return true
}

// stopDetail is what the watchdog saw when a stop round, a floor round or a red one, listed this
// run: the command's footprint and the part of its last period that went to memory stalls,
// against its normal.
func (r *run) stopDetail(floor bool, kib int64, d memsample.TreeTimes) string {
	reason := "memory red"
	if floor {
		reason = "swap room under the floor"
	}
	detail := fmt.Sprintf("%s, at %s", reason, spec.FormatMiB(kib>>10))
	if share, ok := watch.Share(watch.Job{DUserNS: d.UserNS, DSysNS: d.SysNS, DPageWaitNS: d.PageWaitNS}); ok {
		detail += fmt.Sprintf(", %s of its time went to memory stalls against its normal %s", percent(share),
			percent(r.share))
	}
	return detail
}

// percent is a share in whole percent, or to one decimal below 1 %, so that a small normal share
// does not read as none; a share below 0 is 0.
func percent(share float64) string {
	if p := 100 * max(share, 0); p < 0.95 {
		return fmt.Sprintf("%.1f%%", p)
	}
	return fmt.Sprintf("%.0f%%", 100*share)
}

// updateHold rewrites g's hold with fn, outside the section, only while the state directory's
// FORMAT is one this client implements (PROTOCOL.md section 14); it reports whether it wrote.
func (r *run) updateHold(st *store.Store, g *engine.Grant, fn func(*wire.Hold)) bool {
	if err := formatStillUnderstood(st); err != nil {
		return false
	}
	return g.UpdateHold(fn) == nil
}

// treeDeltas returns the time the processes of cur spent since prev. A process new to the tree,
// or whose pid was reused, counts its whole time.
func treeDeltas(prev, cur map[int]memsample.TreeTimes) memsample.TreeTimes {
	var d memsample.TreeTimes
	for pid, cur := range cur {
		was := prev[pid]
		if cur.UserNS < was.UserNS || cur.SysNS < was.SysNS || cur.PageWaitNS < was.PageWaitNS {
			was = memsample.TreeTimes{}
		}
		d.UserNS += cur.UserNS - was.UserNS
		d.SysNS += cur.SysNS - was.SysNS
		d.PageWaitNS += cur.PageWaitNS - was.PageWaitNS
	}
	return d
}

// countClosed adds the tick's time d to the attempt's gate-closed time when det holds a sample the
// tick took or read, its pair was compared (only then is the level green or yellow) and the paging
// gate was closed in it.
func (r *run) countClosed(d memsample.TreeTimes, det watch.Detector, err error) {
	if err == nil && len(det.Window) > 0 && !det.Gate && (det.Level == watch.Green || det.Level == watch.Yellow) {
		r.closed.DUserNS += d.UserNS
		r.closed.DSysNS += d.SysNS
		r.closed.DPageWaitNS += d.PageWaitNS
	}
}

// tickShare is the attempt's share over its gate-closed ticks; nil when none had time.
func (r *run) tickShare() *float64 {
	if share, ok := watch.Share(r.closed); ok {
		return &share
	}
	return nil
}
