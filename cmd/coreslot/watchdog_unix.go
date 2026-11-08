//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/telemetry"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// listingsPerTick is how many of the tree's 1 s listings make one watch tick of the shared period.
const listingsPerTick = int(memsample.PeriodNS / int64(memSampleEvery))

// telemetryTicks is how many of the tree's listings make one live line of a run outside the budget.
var telemetryTicks = 30

// listTree is the listing of the command's tree; tests replace it.
var listTree = memsample.Tree

// listClient scans the command's tree for a container client; tests replace it.
var listClient = memsample.ContainerClient

// watchChild lists the command's tree every second for its memory peak until ctx ends. Every
// second listing, when the run holds g, it writes the tree's footprint, time deltas, CPU time so
// far and smoothed stall share into the hold and takes its part in the shared watch tick; when a
// red round keeps it as nearly finished it says so, and when a stop round lists its hold, it kills
// the command's group and returns stopped, or in observe mode says what it would stop. A command
// named as a container client is container work from the start; any other is from the first
// listing that finds a client in its tree: it takes no stop from then on, and the next tick's hold
// update clears its Victim. The scan needs a log, the one reader that outlives the hold: a
// pass-through has none.
func (r *run) watchChild(ctx context.Context, sup *supervisor, g *engine.Grant, st *store.Store) (stopped bool, p peak) {
	t := time.NewTicker(memSampleEvery)
	defer t.Stop()
	prev := map[int]memsample.TreeTimes{}
	var round, keptRound, cpuNS int64
	var stall float64
	smoothed := false
	p.container = r.o.containerCommand()
	if g != nil { // constant for the attempt, Victim until a hit: written once, before its first tick
		r.hold = g.HoldName()
		r.updateHold(st, g, func(h *wire.Hold) {
			h.Victim, h.Stops, h.NormalShare, h.Learned = sup.group && r.o.restartable(), r.stops, r.share, r.learned
			h.TypicalCPUS, h.Container = r.cpuS, p.container
		})
	}
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return false, p
		case <-t.C:
		}
		kib, times, ok := listTree(sup.pid)
		if ok {
			p.sumKiB, p.samples = p.sumKiB+kib, p.samples+1
			if !p.ok || kib > p.kib {
				p.kib, p.ok = kib, true
			}
		}
		if !p.container && st != nil {
			p.container = listClient(sup.pid)
		}
		if g == nil && st != nil && ok && n%telemetryTicks == 0 { // a pass-through's memory is its parent's
			r.rec.Emit("live", telemetry.Fields{"footprint_mib": kib >> 10})
		}
		if g == nil || n%listingsPerTick != 0 {
			continue
		}
		var d memsample.TreeTimes
		d, prev = treeDeltas(prev, times), times
		// The CPU time so far is the sum of the tick deltas: a process that ended between two
		// listings loses its time since the earlier one, so the progress it gives is low, never high.
		cpuNS += d.UserNS + d.SysNS
		if s, ok := watch.Share(watch.Job{DUserNS: d.UserNS, DSysNS: d.SysNS, DPageWaitNS: d.PageWaitNS}); ok {
			stall, smoothed = watch.Smooth(stall, s, !smoothed, float64(memsample.PeriodNS)/1e9), true
		}
		if !r.updateHold(st, g, func(h *wire.Hold) {
			h.FootprintMiB, h.DUserNS, h.DSysNS, h.DPageWaitNS = kib>>10, d.UserNS, d.SysNS, d.PageWaitNS
			h.CPUNS, h.StallEWMA = cpuNS, stall
			h.SampledNS = store.NowNS()
			if p.container { // every update, so that a failed one repeats by itself
				h.Victim, h.Container = false, true
			}
		}) {
			continue
		}
		det, err := r.tick(ctx, g)
		r.countClosed(d, det, err)
		keptRound = r.keptNotice(det, g.HoldName(), keptRound)
		if err != nil || p.container || det.LastRoundNS == round || !slices.Contains(det.Stop, g.HoldName()) {
			continue
		}
		round = det.LastRoundNS
		if r.stopFor(det, kib, d, keptRound != 0, sup.KillGroup) {
			return true, p
		}
	}
}

// tick takes the run's part in the shared watch tick; a FORMAT the section refused ends its
// recording.
func (r *run) tick(ctx context.Context, g *engine.Grant) (watch.Detector, error) {
	det, err := g.Tick(ctx)
	if errors.Is(err, engine.ErrRefused) {
		r.rec.Stop()
	}
	return det, err
}

// keptNotice says so when det's last round kept the hold, unless the round at lastNS, the last
// one it said so for, was that round; it returns the round it last said so for. A tick that
// failed returned the zero detector, which kept nothing.
func (r *run) keptNotice(det watch.Detector, hold string, lastNS int64) int64 {
	k, ok := det.Kept[hold]
	if !ok || k.RoundNS != det.LastRoundNS || k.RoundNS == lastNS {
		return lastNS
	}
	r.sayKept(det.Observed, k, time.Now())
	r.rec.Emit("kept", r.roundFields(det, "red", hold)) // a floor round keeps nothing
	return k.RoundNS
}

// sayKept says that a red round kept the command as nearly finished, with its progress, the
// estimate the round made and the wall-clock time of the deadline its first keep set (now is the
// time of the round, to the tick), or in observe mode that it would have.
func (r *run) sayKept(observed bool, k watch.Kept, now time.Time) {
	until := now.Add(time.Duration(k.DeadlineNS - k.RoundNS)).Format(time.TimeOnly)
	why := fmt.Sprintf("memory red, about %.0f s left, %s done, until %s", k.LeftS, percent(k.Done), until)
	if observed {
		r.notice("would keep %s (%s)", text(r.o.label), why)
		return
	}
	r.notice("kept %s running (%s)", commandText(r.o.argv), why)
}

// stopFor acts on the stop round det last wrote, which lists this run, as that round was
// written, whatever the level or the mode is now: in observe mode it says what it would stop,
// otherwise it kills the command with kill and reports whether it did. wasKept says that an
// earlier red round kept the command as nearly finished.
func (r *run) stopFor(det watch.Detector, kib int64, d memsample.TreeTimes, wasKept bool, kill func() bool) bool {
	why := r.stopDetail(det.RoundFloor, kib, d)
	if wasKept {
		why += "; an earlier round kept it as nearly finished, this one did not"
	}
	round := "red"
	if det.RoundFloor {
		round = "floor"
	}
	if det.Observed {
		r.notice("would stop %s (%s)", text(r.o.label), why)
		r.rec.Emit("stopped", r.roundFields(det, round, r.hold))
		return false
	}
	if !kill() {
		return false
	}
	r.lastStop, r.lastFloor = why, det.RoundFloor
	r.rec.Emit("stopped", r.roundFields(det, round, r.hold))
	return true
}

// roundFields are the keys the stopped and kept lines share: the numbers behind them are on the
// sample line of the round. list_pos is the hold's place in the round's list, absent for a hold the
// round spared.
func (r *run) roundFields(det watch.Detector, round, hold string) telemetry.Fields {
	f := telemetry.Fields{"round": round, "observe": det.Observed, "level": det.Level.String(), "excess": det.Excess}
	if i := slices.Index(det.Stop, hold); i >= 0 {
		f["list_pos"] = i
	}
	return f
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
		r.rec.Stop()
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
