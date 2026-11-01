//go:build unix

package main

import (
	"context"
	"slices"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
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
func (r *run) watchChild(ctx context.Context, sup *supervisor, g *engine.Grant) (stopped bool, p peak) {
	start := time.Now()
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
			p = peak{kib: kib, afterS: time.Since(start).Seconds(), ok: true}
		}
		if g == nil || n%listingsPerTick != 0 {
			continue
		}
		var d memsample.TreeTimes
		d, prev = treeDeltas(prev, pids)
		_ = g.UpdateHold(func(h *wire.Hold) {
			h.FootprintMiB, h.DUserNS, h.DSysNS, h.DPageWaitNS = kib>>10, d.UserNS, d.SysNS, d.PageWaitNS
			h.SampledNS, h.Victim, h.Stops = store.NowNS(), sup.group && !r.o.noRestart, r.stops
		})
		det, c, err := g.Tick(ctx, r.normal)
		if err != nil || det.LastRoundNS == round || !slices.Contains(det.Stop, g.HoldName()) {
			continue
		}
		round = det.LastRoundNS
		if !c.Observe && sup.KillGroup() {
			return true, p
		}
		if c.Observe {
			reason := "memory red"
			if det.Level == watch.Floor {
				reason = "swap room under the floor"
			}
			r.notice("would stop %s (%s)", text(r.o.label), reason)
		}
	}
}

// treeDeltas is the time the processes pids spent since prev, their times then, and their times
// now. A process new to the tree, or whose pid was reused, counts its whole time.
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
