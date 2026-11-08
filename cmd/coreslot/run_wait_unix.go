//go:build unix

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/telemetry"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// errNotAtOnce is the outcome of a --no-wait attempt that was not admitted.
var errNotAtOnce = errors.New("not admitted at once")

// tryOnce is --no-wait's single attempt: one Step, each of whose section entries waits at most
// RefreshEvery/4 = 0.5 s (the engine's sectionWait). The 1 s of noWaitFor bounds only enqueue.
func (r *run) tryOnce(ctx context.Context, t *engine.Ticket) (*engine.Grant, error) {
	res, err := r.stepperOf(t).step(ctx)
	if err != nil {
		return nil, err
	}
	if a, ok := res.(engine.Admitted); ok {
		return a.Grant, nil
	}
	return nil, errNotAtOnce
}

// stepper is the part of a ticket the wait loop drives: a Step, and how long until the next one
// has work due.
type stepper interface {
	step(ctx context.Context) (engine.StepResult, error)
	due() time.Duration
}

// ticketStepper steps a real ticket.
type ticketStepper struct{ t *engine.Ticket }

func (s ticketStepper) step(ctx context.Context) (engine.StepResult, error) { return s.t.Step(ctx) }

func (s ticketStepper) due() time.Duration { return s.t.Due() }

// stepperOf is what steps t: the ticket itself, or with a recorder the ticket traced.
func (r *run) stepperOf(t *engine.Ticket) stepper {
	if r.rec == nil {
		return ticketStepper{t}
	}
	r.trace = tracedStepper{stepper: ticketStepper{t}, clk: newRealClock()}
	return &r.trace
}

// tracedStepper keeps where a wait's time went. The time between two steps is charged to the
// first one's reason: that is what the run waited on meanwhile.
type tracedStepper struct {
	stepper
	clk     waitClock
	stepped bool
	at      time.Duration // when the last step returned
	reason  string        // the last step's
	secs    map[string]float64
	maxGap  time.Duration
}

func (s *tracedStepper) step(ctx context.Context) (engine.StepResult, error) {
	res, err := s.stepper.step(ctx)
	now := s.clk.Now()
	if gap := now - s.at; s.stepped {
		s.maxGap = max(s.maxGap, gap)
		if s.reason != "" {
			if s.secs == nil {
				s.secs = map[string]float64{}
			}
			s.secs[s.reason] += gap.Seconds()
		}
	}
	s.at, s.stepped = now, true
	if w, ok := res.(engine.Waiting); ok {
		s.reason = string(w.Outcome.Reason)
	}
	return res, err
}

// take returns the seconds per reason, to the millisecond a line is worth, and clears them.
func (s *tracedStepper) take() map[string]float64 {
	out := make(map[string]float64, len(s.secs))
	for why, v := range s.secs {
		out[why] = math.Round(v*1e3) / 1e3
	}
	s.secs = nil
	return out
}

// takeGap returns the longest time between two steps and clears it.
func (s *tracedStepper) takeGap() time.Duration {
	g := s.maxGap
	s.maxGap = 0
	return g
}

// waitOneP runs wait with the Go runtime limited to one P (runtime.GOMAXPROCS(1)) and restores
// the previous setting afterwards. A waiting process sleeps almost all the time, and with one P
// its wake-ups do not wake idle threads of the other Ps, which cuts a waiter's own cost
// (bench/floor measures it). Only this process changes: GOMAXPROCS is not exported to the command.
func waitOneP(wait func() (*engine.Grant, error)) (*engine.Grant, error) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	return wait()
}

// waitClock is the wait loop's clock: monotonic time and a sleep that ends with ctx.
type waitClock interface {
	Now() time.Duration
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{ start time.Time }

func newRealClock() realClock { return realClock{start: time.Now()} }

func (c realClock) Now() time.Duration { return time.Since(c.start) }

func (c realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// waitLoop steps the ticket whenever it next has work due (once per second while it is not the
// head, on the admission backoff while it is) until it is admitted, ctx ends or the budget
// fails, and writes a waiting line to w when the wait starts and every 30 s after.
func waitLoop(ctx context.Context, s stepper, clk waitClock, w io.Writer, line func(engine.Waiting, time.Duration) string) (*engine.Grant, error) {
	start := clk.Now()
	next := start
	for {
		res, err := s.step(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		switch v := res.(type) {
		case engine.Admitted:
			return v.Grant, nil
		case engine.Waiting:
			if now := clk.Now(); now >= next {
				fmt.Fprintln(w, line(v, now-start))
				next = now + waitLineEvery
			}
		}
		if err := clk.Sleep(ctx, s.due()); err != nil {
			return nil, err
		}
	}
}

// waitingLine is the line a waiting run prints every 30 s, for example `coreslot: waiting for
// mem 3 GiB: position 2 (1m00s); holders: gate pid 4711 mem 1 GiB (3m12s)`, where 1m00s is how
// long the run has waited. Why it waits and the holds come from one snapshot.
func (r *run) waitingLine(w engine.Waiting, waited time.Duration) string {
	why, advice := "", ""
	var holders []engine.HolderInfo
	if r.why != nil {
		if why, holders = r.why(); why != "" {
			why = "; " + why
		}
	}
	r.recordWaiting(w)
	if !r.advised {
		r.advised, advice = true, "; if your tool has a time limit, run this in the background or raise the limit"
	}
	place := fmt.Sprintf(": position %d", w.Position)
	if r.o.request().RealMemory && len(holders) > 0 { // it waits on neither the queue nor the holds
		place, holders = "", nil
	}
	return fmt.Sprintf("coreslot: waiting for %s%s (%s)%s%s%s%s", r.o.needText(), place,
		duration(waited), why, holderList(holders, time.Now().UnixNano()), r.pipeNote(), advice)
}

// recordEnqueue writes the attempt's enqueue, making the run's recorder on its first.
func (r *run) recordEnqueue(e *engine.Engine, t *engine.Ticket, req spec.Request) {
	if !r.telemetry {
		return
	}
	if r.rec == nil {
		if r.rec = telemetry.New(r.loc.dir, fmt.Sprintf("%d-%d", r.start.UnixNano(), os.Getpid()), r.o.label); r.rec == nil {
			return
		}
	}
	r.rec.SetAttempt(r.stops + 1)
	f := telemetry.Fields{"ticket": t.Name(), "need_mib": req.NeedMiB, "need_source": r.o.needSource, "real_memory": req.RealMemory,
		"typical_s": req.TypicalS, "oversized": r.oversized, "timeout_s": r.o.timeout.Seconds(), "on_timeout": r.o.onTimeout,
		"version": cliVersion(), "mem_mib": r.memMiB}
	if r.o.needSource == "restart_peak" {
		f["need_before_mib"] = r.needBefore
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
	r.rec.Emit("enqueue", f)
}

// recordWaiting writes a waiting line beside the one a person reads, from the same snapshot.
func (r *run) recordWaiting(w engine.Waiting) {
	if r.rec == nil {
		return
	}
	out := w.Outcome
	f := telemetry.Fields{"reason": string(out.Reason), "reason_s": r.trace.take(), "evals": w.Evals, "max_gap_ms": r.trace.takeGap().Milliseconds()}
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
	if rep := r.snap; rep != nil {
		f["headroom_mib"], f["headroom_ok"], f["reserve_mib"] = rep.Mem.HeadroomMiB, rep.Mem.HeadroomOK, rep.Mem.ReserveMiB
		f["room_mib"], f["holders"] = waitRoom(*rep, r.o.request()), holderLines(rep.Holds)
		if rep.Watch.SampledNS != 0 { // with no sample the zero level would read as green
			f["level"], f["read_age_ms"] = rep.Watch.Level.String(), max(0, store.NowNS()-rep.Watch.SampledNS)/int64(time.Millisecond)
		}
	}
	r.rec.Emit("waiting", f)
}

// recordAdmitted writes the attempt's admission and what the decision read.
func (r *run) recordAdmitted(g *engine.Grant) {
	if r.rec == nil {
		return
	}
	a := g.Admission()
	f := telemetry.Fields{"hold": g.HoldName(), "waited_s": r.waited.Seconds(), "reason_s": r.trace.take(), "last_reason": r.trace.reason,
		"headroom_mib": a.HeadroomMiB, "headroom_ok": a.HeadroomOK, "reserve_mib": a.ReserveMiB, "room_mib": a.RoomMiB, "watch_gate": a.WatchGate}
	if a.WKnown {
		f["w_s"] = a.W
	}
	r.rec.Emit("admitted", f)
}

// maxHolderLines bounds the holders of a waiting line, so that the line fits MaxRunLine.
const maxHolderLines = 16

// holderLines describes the holds on a waiting line, those with the most left to grow into first.
func holderLines(holds []engine.HolderInfo) []map[string]any {
	holds = slices.Clone(holds)
	growth := func(h engine.HolderInfo) int64 { return max(0, h.NeedMiB-h.FootprintMiB) }
	slices.SortStableFunc(holds, func(a, b engine.HolderInfo) int { return cmp.Compare(growth(b), growth(a)) })
	holds = holds[:min(len(holds), maxHolderLines)]
	out := make([]map[string]any, 0, len(holds))
	for _, h := range holds {
		l := map[string]any{"hold": h.Name, "need_mib": h.NeedMiB, "footprint_mib": h.FootprintMiB, "child_exited": h.ChildExited, "owner_exited": h.OwnerExited}
		if h.SampleAgeMS >= 0 {
			l["sample_age_ms"] = h.SampleAgeMS
		}
		out = append(out, l)
	}
	return out
}

// waitRoom is the room a waiting request is judged against. A container request's is not FreeMiB,
// which subtracts the reservations it ignores.
func waitRoom(rep engine.Report, req spec.Request) int64 {
	if req.RealMemory {
		return rep.Mem.RealFreeMiB
	}
	return rep.Mem.FreeMiB
}

// waitReason says why a waiting request is not admitted: the room it lacks, else the memory level
// that holds admissions, else that a restarted command waits for calm.
func waitReason(rep engine.Report, req spec.Request) string {
	w := rep.Watch
	free := waitRoom(rep, req)
	switch {
	case rep.Mem.Known && len(rep.Holds) > 0 && req.NeedMiB > free:
		return fmt.Sprintf("needs %s, %s free", spec.FormatMiB(req.NeedMiB), spec.FormatMiB(free))
	case w.Fresh && w.Level >= watch.Yellow:
		return "memory is " + w.Level.String()
	case req.Restart:
		return "a stopped command restarts when memory has been calm for a while"
	}
	return ""
}

// pipeNote is what the first waiting line adds when the run's stdin is a pipe: the command
// writing it may be a stage of the same pipeline that holds the memory this run waits for, and
// blocks on the full pipe until this run reads it, so the two wait on each other until a
// timeout. coreslot cannot see the other end of a pipe, so it says so once rather than deciding.
func (r *run) pipeNote() string {
	if r.pipeNoted || r.io.Stdin == nil {
		return ""
	}
	r.pipeNoted = true
	if fi, err := r.io.Stdin.Stat(); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		return ""
	}
	return "; stdin is a pipe: if the command writing it holds a grant, it may be waiting for this run " +
		"to read; run the whole pipeline under one coreslot run"
}

// holdersText describes the live holds and the section's holder, for a --no-wait refusal.
func (r *run) holdersText(e *engine.Engine) string {
	rep, err := e.Snapshot()
	if err != nil {
		return ""
	}
	s := ""
	if !r.o.request().RealMemory { // a container request does not wait on them
		s = holderList(rep.Holds, time.Now().UnixNano())
	}
	if rep.Section.Held && rep.Section.PID != 0 {
		s += fmt.Sprintf("; the admission section is held by pid %d", rep.Section.PID)
	} else if rep.Section.Held {
		s += "; the admission section is held"
	}
	return s
}

// holderList is `; holders: gate pid 4711 mem 3 GiB (3m12s), ...`, or "" without holders. Labels
// are escaped so that each holder stays on the line.
func holderList(holders []engine.HolderInfo, nowNS int64) string {
	if len(holders) == 0 {
		return ""
	}
	parts := make([]string, 0, len(holders))
	for _, h := range holders {
		label := wire.EscapeHuman(h.Label)
		if label == "" {
			label = "(no label)"
		}
		parts = append(parts, fmt.Sprintf("%s pid %d %s (%s)", label, h.PID, holdNeed(h),
			duration(time.Duration(nowNS-h.StartNS))))
	}
	return "; holders: " + strings.Join(parts, ", ")
}

// holdNeed is what a hold reserves for a person: `mem 3 GiB`, or `unbudgeted` for a run outside
// the budget, which reserves nothing.
func holdNeed(h engine.HolderInfo) string {
	if h.NeedMiB == 0 {
		return "unbudgeted"
	}
	return "mem " + spec.FormatMiB(h.NeedMiB)
}

// duration is a coarse duration: 12s, 3m12s, 1h02m03s.
func duration(d time.Duration) string {
	s := int64(max(d, 0) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm%02ds", s/3600, s%3600/60, s%60)
}
