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

// listingsPerTick is how many of the tree's 1 s listings make one watch tick: the shared period
// is 2 s.
const listingsPerTick = 2

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
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return false, p
		case <-t.C:
		}
		kib, pids, ok := listTree(sup.pid)
		if ok && (!p.ok || kib > p.kib) {
			p = peak{kib: kib, ok: true}
		}
		if g == nil || n%listingsPerTick != 0 {
			continue
		}
		var d memsample.TreeTimes
		d, prev = treeDeltas(prev, pids)
		if !r.updateHold(st, g, func(h *wire.Hold) {
			h.FootprintMiB, h.DUserNS, h.DSysNS, h.DPageWaitNS = kib>>10, d.UserNS, d.SysNS, d.PageWaitNS
			h.SampledNS, h.Victim, h.Stops = store.NowNS(), sup.group && !r.o.noRestart, r.stops
		}) {
			continue
		}
		det, c, err := g.Tick(ctx, r.normal)
		if err != nil || det.LastRoundNS == round || !slices.Contains(det.Stop, g.HoldName()) {
			continue
		}
		round = det.LastRoundNS
		why := r.stopDetail(det.Level, kib, d)
		if c.Observe {
			r.notice("would stop %s (%s)", text(r.o.label), why)
		} else if sup.KillGroup() {
			r.lastStop = why
			return true, p
		}
	}
}

// stopDetail is what the watchdog saw when a stop round listed this run: the memory level, the
// command's footprint and the part of its last period that went to memory stalls, against its
// normal.
func (r *run) stopDetail(l watch.Level, kib int64, d memsample.TreeTimes) string {
	reason := "memory " + l.String()
	if l == watch.Floor {
		reason = "swap room under the floor"
	}
	detail := fmt.Sprintf("%s, at %s", reason, spec.FormatMiB(kib>>10))
	if share, ok := watch.Share(watch.Job{DUserNS: d.UserNS, DSysNS: d.SysNS, DPageWaitNS: d.PageWaitNS}); ok {
		normal, known := r.normal[r.o.label]
		if !known {
			normal = watch.DefaultNormalShare
		}
		detail += fmt.Sprintf(", %.0f%% of its time went to memory stalls against its normal %.0f%%", 100*share, 100*normal)
	}
	return detail
}

// updateHold rewrites g's hold with fn, outside the section, only while the state directory's
// FORMAT is one this client implements (PROTOCOL.md section 14); it reports whether it wrote.
func (r *run) updateHold(st *store.Store, g *engine.Grant, fn func(*wire.Hold)) bool {
	if err := formatStillUnderstood(st); err != nil {
		return false
	}
	return g.UpdateHold(fn) == nil
}

// treeDeltas returns the time the processes pids spent since prev, their times then, and their
// times now. A process new to the tree, or whose pid was reused, counts its whole time.
func treeDeltas(prev map[int]memsample.TreeTimes, pids []int) (memsample.TreeTimes, map[int]memsample.TreeTimes) {
	var d memsample.TreeTimes
	next := make(map[int]memsample.TreeTimes, len(pids))
	for _, pid := range pids {
		cur, err := memsample.ReadTreeTimes([]int{pid})
		if err != nil {
			continue
		}
		next[pid] = cur
		was := prev[pid]
		if cur.UserNS < was.UserNS || cur.SysNS < was.SysNS || cur.PageWaitNS < was.PageWaitNS {
			was = memsample.TreeTimes{}
		}
		d.UserNS += cur.UserNS - was.UserNS
		d.SysNS += cur.SysNS - was.SysNS
		d.PageWaitNS += cur.PageWaitNS - was.PageWaitNS
	}
	return d, next
}
