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
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/telemetry"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// listingsPerTick is how many of the tree's 1 s listings make one watch tick of the shared period.
const listingsPerTick = int(memsample.PeriodNS / int64(memSampleEvery))

// telemetryTicks is how many of the tree's listings make one telemetry.Cadence.
var telemetryTicks = int(telemetry.Cadence / memSampleEvery)

// The no-CPU mark (no_cpu_s, PROTOCOL.md section 7.4): noCPUWindowTicks ticks of 2 s make a window
// of 5 min, and noCPUMarkWindows of them in a row the 30 min from which a hold says so.
var noCPUWindowTicks, noCPUMarkWindows = int(5 * time.Minute / time.Duration(memsample.PeriodNS)), int64(6)

// noCPUMeter counts a tree's quiet windows in a row. It counts ticks, not a clock's time, so that a
// machine asleep adds nothing. It only informs: nothing but status reads what it gives.
type noCPUMeter struct {
	ticks   int   // of the window under way
	busyNS  int64 // the tree's time in that window
	active  bool  // that window showed activity that its time may not
	windows int64 // quiet windows in a row
}

// add takes a tick's times and whether the tick showed activity they may not, and returns the
// seconds to write to the hold, 0 under the mark. A window is quiet when the tree spent under 1 % of
// a core in it, no tick was active and client finds no container client in the tree at its end, as
// a client's CPU says nothing of its containers' work. The mark goes once the window under way is
// not.
func (m *noCPUMeter) add(d memsample.TreeTimes, active bool, client func() bool) int64 {
	m.ticks++
	m.busyNS += d.UserNS + d.SysNS + d.PageWaitNS
	m.active = m.active || active
	windowNS := int64(noCPUWindowTicks) * memsample.PeriodNS
	quiet := m.busyNS < windowNS/100 && !m.active
	if m.ticks == noCPUWindowTicks {
		if quiet = quiet && !client(); quiet {
			m.windows++
		}
		m.ticks, m.busyNS, m.active = 0, 0, false
	}
	if !quiet {
		m.windows = 0
	}
	if m.windows < noCPUMarkWindows {
		return 0
	}
	return m.windows * windowNS / int64(time.Second)
}

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
// listing that finds a client in its tree, or from its forwarder's first connection: it takes no
// stop from then on, and the next tick's hold update clears its Victim. The scan stays under a
// forwarder, for a client that bypasses it. The scan needs a log, the one reader that outlives the
// hold: a pass-through has none. An engaged run starts a sample of its containers every third tick,
// a gated one every tick until they reach its Docker need, and every hold write carries the last
// sample done. A hold that reserves memory keeps a window over the listings that succeeded from the
// attempt's start. While the attempt follows (follows), or once a tick finds a request to trim it in
// trim.json, every later tick writes the window's need into the hold, never above the need asked; a
// trim shortens the window to the trim window. A following run that becomes container work stops
// following and writes the need asked from then on.
func (r *run) watchChild(ctx context.Context, sup *supervisor, g *engine.Grant, st *store.Store) (stopped bool, p peak) {
	t := time.NewTicker(memSampleEvery)
	defer t.Stop()
	prev := map[int]memsample.TreeTimes{}
	var round, keptRound int64
	var stall float64
	var noCPU noCPUMeter
	smoothed := false
	p.container = r.o.containerCommand()
	var tail *state.Tail // the attempt's readings; nil for a run that reserves nothing
	var trim struct {
		found bool // a request names the hold: it holds for the attempt
		said  bool // a write of the trimmed need succeeded and the run said so
	}
	readings := func(s int64) int { return int(time.Duration(s) * time.Second / memSampleEvery) }
	follow, reset := false, false // reset: the run followed and became container work
	if g != nil && g.GrantMiB() > 0 {
		window := r.trimWindowS
		if follow = r.follows(g); follow {
			window = r.windowS
		}
		tail = state.NewTail(g.GrantMiB(), readings(window))
		p.askedMiB = g.GrantMiB()
	}
	// A do-not-stop mark keeps the hold out of stop rounds. Once the mark goes, a run that refused
	// round R stays out until a tick reads a round other than R: R's listing would otherwise stay
	// live and hold back every new round (PROTOCOL.md section 7.18).
	var noStop struct {
		on       bool  // the run writes Victim false
		refused  int64 // the round it refused while marked; 0 for none
		lastRead int64 // the round the last tick read; 0 when it read none
	}
	victim := func() bool { return !noStop.on && !p.container && sup.group && r.o.restartable() }
	if g != nil { // constant for the attempt, Victim until a hit or a mark: written once, before its first tick
		r.hold = g.HoldName()
		noStop.on = g.NoStop()
		r.updateHold(st, g, func(h *wire.Hold) {
			h.Victim, h.Stops, h.NormalShare, h.Learned = victim(), r.stops, r.share, r.learned
			h.TypicalCPUS, h.Container = r.cpuS, p.container
			h.DockerNote, h.DockerFootprintMiB = r.dockerNote(), r.dockerFootprint()
			r.setDockerKeys(g, h)
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
			if tail != nil { // a listing that failed is no reading
				tail.Add(kib >> 10)
				p.readings = append(p.readings, int32(kib>>10))
			}
		}
		if r.fwd != nil {
			r.forwarderEvents(&p)
			if r.fwd.part.touching() { // off the watchdog's loop, as the sample is
				go r.fwd.part.readTouches(r.fwd.upstream)
			}
		}
		if !p.container && st != nil {
			p.container = listClient(sup.pid)
		}
		if st != nil && n%telemetryTicks == 0 { // a pass-through's memory is its parent's, and it has no log
			if g == nil && ok {
				r.record.Emit("live", telemetry.Fields{"footprint_mib": kib >> 10})
			}
			r.record.MemoryAsync()
			if p.container {
				r.record.ContainersAsync(r.sockets, r.dockerOff)
			}
		}
		if g == nil || n%listingsPerTick != 0 {
			continue
		}
		p.tick(kib, ok)
		d, fresh := treeDeltas(prev, times)
		prev = times
		// A child born and reaped between two ticks leaves no time a tick sees, so a tree that keeps
		// making processes never counts as quiet: a wait loop that forks stays unmarked.
		active := !ok || fresh
		// The CPU time so far is the sum of the tick deltas: a process that ended between two
		// listings loses its time since the earlier one, so the progress it gives is low, never high.
		p.cpuNS, p.cpuTicks = p.cpuNS+d.UserNS+d.SysNS, p.cpuTicks+1
		if s, ok := watch.Share(watch.Job{DUserNS: d.UserNS, DSysNS: d.SysNS, DPageWaitNS: d.PageWaitNS}); ok {
			stall, smoothed = watch.Smooth(stall, s, !smoothed, float64(memsample.PeriodNS)/1e9), true
		}
		noCPUS := noCPU.add(d, active, func() bool { return p.container && listClient(sup.pid) }) // a run not yet container work scanned this listing
		if r.fwd != nil && ((n/listingsPerTick)%sampleTicks == 0 || r.growing()) {
			// Off the tick path: a hung engine never delays a tick, and the hold takes the last
			// sample done.
			go sampleDocker(ctx, r.fwd, r.gate)
		}
		// The request is read after the tick has measured, until one is found.
		if tail != nil && !trim.found {
			if trim.found = g.TrimRequested(); trim.found {
				tail.Shorten(readings(r.trimWindowS))
			}
		}
		if follow && p.container {
			follow, reset = false, true
		}
		var need int64
		if follow || trim.found {
			need = tail.Need()
		}
		if g.NoStop() {
			noStop.on = true
		} else if noStop.on && (noStop.refused == 0 || noStop.lastRead > 0 && noStop.lastRead != noStop.refused) {
			noStop.on, noStop.refused = false, 0
		}
		wrote := r.updateHold(st, g, func(h *wire.Hold) {
			h.FootprintMiB, h.DUserNS, h.DSysNS, h.DPageWaitNS = kib>>10, d.UserNS, d.SysNS, d.PageWaitNS
			h.CPUNS, h.StallEWMA, h.NoCPUS = p.cpuNS, stall, noCPUS
			h.SampledNS, h.DockerNote, h.DockerFootprintMiB = store.NowNS(), r.dockerNote(), r.dockerFootprint()
			r.setDockerKeys(g, h)
			h.Victim = victim() // every update, so that a failed one repeats by itself
			if p.container {
				h.Container = true
			}
			if reset { // every update, so that a failed one repeats; a trim's write below wins
				h.NeedMiB, h.AskedMiB = g.GrantMiB(), 0
			}
			if follow || trim.found { // every update, as the footprint it follows
				h.NeedMiB, h.AskedMiB, h.Trimmed = need, g.GrantMiB(), trim.found
			}
		})
		if wrote && trim.found && !trim.said {
			trim.said = true
			r.sayTrimmed(g, need, kib>>10)
		}
		if !wrote {
			continue
		}
		det, err := g.Tick(ctx)
		noStop.lastRead = det.LastRoundNS // the zero detector of a failed tick reads no round
		r.countClosed(d, det, err)
		keptRound = r.keptNotice(det, g.HoldName(), keptRound)
		if err != nil || p.container || det.LastRoundNS == round || !slices.Contains(det.Stop, g.HoldName()) {
			continue
		}
		round = det.LastRoundNS
		if g.NoStop() { // marked since the run's last write: the round is refused, as if acted on
			noStop.on, noStop.refused = true, round
			r.notice("%s is marked do-not-stop: not stopped (%s)", commandText(r.o.argv), r.stopDetail(det.RoundFloor, kib, d))
			continue
		}
		if r.stopFor(det, kib, d, keptRound != 0, sup.KillGroup) {
			return true, p
		}
	}
}

// follows reports whether the attempt on g follows what its run uses (PROTOCOL.md section 12.5):
// its need was not pinned, it had no Docker interest at its start, its grant is within the bound
// (not oversized) and it is the run's first attempt, as a restart after a stop reuses the window
// that may have missed the regrowth it was stopped for.
func (r *run) follows(g *engine.Grant) bool {
	return !r.o.pinned && !r.dockerInterest() && g.GrantMiB() <= r.boundMiB && r.stops == 0
}

// sayTrimmed says that the attempt's hold now reserves needMiB, lowered by hand with `coreslot
// trim`, and follows its command's footprint, and records it: once, at the first write of it.
func (r *run) sayTrimmed(g *engine.Grant, needMiB, footprintMiB int64) {
	r.notice("the reservation of %s was lowered by hand with coreslot trim: it follows what the command uses, up to the %s it asked for (%s now)",
		commandText(r.o.argv), spec.FormatMiB(g.GrantMiB()), spec.FormatMiB(needMiB))
	r.record.trimmed(g.HoldName(), g.GrantMiB(), needMiB, footprintMiB)
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
	r.record.Emit("kept", r.roundFields(det, "red", hold)) // a floor round keeps nothing
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
		r.record.Emit("stopped", r.roundFields(det, round, r.hold))
		return false
	}
	if !kill() {
		return false
	}
	r.lastStop, r.lastFloor = why, det.RoundFloor
	r.record.Emit("stopped", r.roundFields(det, round, r.hold))
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
	return formatStillUnderstood(st) == nil && g.UpdateHold(fn) == nil
}

// treeDeltas returns the time the processes of cur spent since prev, and whether cur holds a
// process new to the tree. A new process, or one whose pid was reused, counts its whole time.
func treeDeltas(prev, cur map[int]memsample.TreeTimes) (d memsample.TreeTimes, fresh bool) {
	for pid, cur := range cur {
		was, seen := prev[pid]
		if !seen || cur.UserNS < was.UserNS || cur.SysNS < was.SysNS || cur.PageWaitNS < was.PageWaitNS {
			was, fresh = memsample.TreeTimes{}, true
		}
		d.UserNS += cur.UserNS - was.UserNS
		d.SysNS += cur.SysNS - was.SysNS
		d.PageWaitNS += cur.PageWaitNS - was.PageWaitNS
	}
	return d, fresh
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
