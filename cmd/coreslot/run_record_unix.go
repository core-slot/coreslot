//go:build unix

package main

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/telemetry"
)

// runRecord is one run's part of the local record. Its recorder is nil with telemetry off, and
// then the methods here and the recorder's own write nothing, so a call site says what happened
// and never asks whether the run records. The recorder reads FORMAT before every line: a call site
// stops it only for what FORMAT cannot show, a release that found its files replaced or a run
// that published nothing to join its lines to.
type runRecord struct {
	*telemetry.Recorder
	run     *run
	trace   tracedStepper  // where the attempt's wait went
	snap    *engine.Report // the last waiting line's snapshot
	overMiB int64          // above this need a run can never fit beside another job
	queued  bool           // this attempt's enqueue was recorded: its wait has an end to report
}

// arm starts the record of r, whose first line is still to come.
func (x *runRecord) arm(r *run, overMiB int64) {
	*x = runRecord{Recorder: telemetry.New(r.loc.dir, fmt.Sprintf("%d-%d", r.start.UnixNano(), os.Getpid()), r.o.label), run: r, overMiB: overMiB}
}

// stepper is what steps t: the ticket itself, or with a recorder the ticket traced.
func (x *runRecord) stepper(t *engine.Ticket) stepper {
	if x.Recorder == nil {
		return ticketStepper{t}
	}
	x.trace.stepper = ticketStepper{t}
	return &x.trace
}

// enqueue writes the attempt's enqueue.
func (x *runRecord) enqueue(e *engine.Engine, t *engine.Ticket, req spec.Request) {
	if x.Recorder == nil {
		return
	}
	r := x.run
	x.trace = tracedStepper{clk: newRealClock(), at: -time.Since(r.start)} // from here a wait can end, and be charged
	x.SetAttempt(r.stops + 1)
	f := telemetry.Fields{"ticket": t.Name(), "need_mib": req.NeedMiB, "need_source": r.o.needSource, "real_memory": req.RealMemory,
		"typical_s": req.TypicalS, "oversized": req.NeedMiB > x.overMiB, "timeout_s": r.o.timeout.Seconds(), "on_timeout": r.o.onTimeout,
		"version": cliVersion(), "mem_mib": r.memMiB}
	if r.nestedMiss != "" {
		f["nested_miss"] = r.nestedMiss
	}
	if r.parentHold != "" {
		f["parent_hold"] = r.parentHold
	}
	if c, err := e.Config(); err == nil {
		mode := "enforce"
		if c.Watch.Observe {
			mode = "observe"
		}
		w := c.Watch
		f["mode"], f["reserve_mib"], f["watch_config"] = mode, c.ReserveMiB, map[string]any{
			"yellow_points": w.YellowPoints, "red_points": w.RedPoints, "red_samples": w.RedSamples, "gate_pages_s": w.GatePagesPerS,
			"floor_mib": w.FloorMiB, "calm_s": w.CalmS, "post_kill_s": w.PostKillS, "spacing_s": w.SpacingS}
	}
	x.Emit("enqueue", f)
	x.queued = true
}

// waiting writes a waiting line beside the one a person reads, from the same snapshot.
func (x *runRecord) waiting(w engine.Waiting) {
	if x.Recorder == nil {
		return
	}
	out := w.Outcome
	gap := x.trace.maxGap
	f := telemetry.Fields{"reason": string(out.Reason), "reason_s": x.trace.take(), "evals": w.Evals, "max_gap_ms": gap.Milliseconds()}
	if w.Ahead != "" {
		f["ahead"] = w.Ahead
	}
	if out.NoPass != "" {
		f["no_pass"] = out.NoPass
	}
	if out.WKnown {
		f["w_s"] = out.W
	}
	if out.Reason == engine.ReasonRestartCalm {
		f["calm_left_s"] = out.CalmLeftS
	}
	if rep := x.snap; rep != nil {
		f["headroom_mib"], f["headroom_ok"], f["reserve_mib"] = rep.Mem.HeadroomMiB, rep.Mem.HeadroomOK, rep.Mem.ReserveMiB
		f["room_mib"], f["holders"] = waitRoom(*rep, x.run.o.request()), holderLines(rep.Holds)
		if rep.Watch.SampledNS != 0 { // with no sample the zero level would read as green
			f["level"], f["read_age_ms"] = rep.Watch.Level.String(), max(0, store.NowNS()-rep.Watch.SampledNS)/int64(time.Millisecond)
		}
	}
	x.Emit("waiting", f)
	x.MemoryAsync()
}

// admitted writes the attempt's admission and what the decision read.
func (x *runRecord) admitted(g *engine.Grant) {
	if x.Recorder == nil {
		return
	}
	a := g.Admission()
	f := telemetry.Fields{"hold": g.HoldName(), "waited_s": x.run.waited.Seconds(), "reason_s": x.trace.take(), "last_reason": x.trace.reason,
		"headroom_mib": a.HeadroomMiB, "headroom_ok": a.HeadroomOK, "reserve_mib": a.ReserveMiB, "room_mib": a.RoomMiB, "watch_gate": a.WatchGate}
	if a.WKnown {
		f["w_s"] = a.W
	}
	x.Emit("admitted", f)
}

// waitEnd writes event, the end of a wait that gave no start, with where the wait went.
func (x *runRecord) waitEnd(event string, f telemetry.Fields) {
	if x.Recorder == nil || !x.queued {
		return
	}
	x.closeWait(f)
	x.Emit(event, f)
}

// closeWait adds to f where a wait that gave no start went.
func (x *runRecord) closeWait(f telemetry.Fields) {
	x.trace.charge() // such a wait ends in its sleep, after the last step
	f["waited_s"], f["reason_s"], f["last_reason"] = x.run.waited.Seconds(), x.trace.take(), x.trace.reason
}

// outside writes the line of a run with no ticket, or whose ticket is gone: kind and the closed
// reason, with the parent's hold when it has one.
func (x *runRecord) outside(kind, reason, parent string) {
	if x.Recorder == nil {
		return
	}
	f := telemetry.Fields{"kind": kind, "reason": reason, "need_mib": x.run.o.need}
	if parent != "" {
		f["parent"] = parent
	}
	if kind == "fail_open" && x.queued { // no other line closes this wait
		x.closeWait(f)
	}
	x.SetAttempt(x.run.stops + 1)
	x.Emit("outside", f)
}

// ended writes the attempt's outcome: the run-log line and what that line cannot say.
func (x *runRecord) ended(c child, rec store.RunRecord, sup *supervisor, p peak, killed bool) {
	if x.Recorder == nil {
		return
	}
	rec.FailOpen = ""
	f := telemetry.Fields{"record": rec}
	if c.hold != "" {
		f["hold"] = c.hold
	}
	if c.failCode != "" {
		f["fail_open"] = c.failCode
	}
	if sup.ws.Signaled() {
		f["signal"] = unix.SignalName(sup.ws.Signal())
	}
	switch {
	case x.run.stopped && x.run.lastFloor:
		f["stopped_by"] = "watchdog_floor"
	case x.run.stopped:
		f["stopped_by"] = "watchdog_red"
	case killed:
		f["stopped_by"] = "outside"
	}
	if p.samples > 0 {
		f["mean_footprint_mib"] = p.sumKiB / p.samples / 1024
	}
	x.Emit("ended", f)
}
