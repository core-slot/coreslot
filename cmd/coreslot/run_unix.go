//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/runstats"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/telemetry"
	"github.com/core-slot/coreslot/internal/watch"
)

// Timing of a waiting run (PROTOCOL.md section 22).
const (
	waitLineEvery = 30 * time.Second // the waiting line
	noWaitFor     = time.Second      // --no-wait: how long the section is tried
)

// maxStops is the memory stops after which a run gives up.
const maxStops = 3

// readLog reads the run log once for a run: the need it gives label, the p90 peak of its last
// counted runs (ok false when unknown), the label's normal share of system time (learned false
// when none of those runs has a tick share), its typical run time (0 when unknown) and its typical
// CPU seconds (0 when too few of those runs report them).
func readLog(loc location, label string) (needMiB int64, ok bool, share float64, learned bool, typicalS int64, typicalCPUS float64) {
	if loc.err != nil {
		return
	}
	st, err := store.OpenReadOnly(loc.dir)
	if err != nil || !st.Exists() {
		return
	}
	runs, _, err := st.ReadRuns()
	if err != nil {
		return
	}
	window := runstats.Window(runs, label, runstats.WindowRuns)
	needMiB, ok = runstats.NeedMiB(window)
	share, learned = runstats.NormalShare(window)
	typicalS = runstats.TypicalS(window)
	typicalCPUS = runstats.TypicalCPUS(window)
	return
}

// run is one `coreslot run`.
type run struct {
	o         runOptions
	io        IO
	loc       location  // the state directory and configuration, located once
	start     time.Time // the attempt's start: the run's, or its re-enqueue after a stop
	waited    time.Duration
	env       []string                             // the command's environment before its grant: the inherited context dropped
	pipeNoted bool                                 // the waiting line said once whether stdin is a pipe
	share     float64                              // the label's normal share, from the run log read at the start
	learned   bool                                 // the run log gave the label a normal share
	cpuS      float64                              // the label's typical CPU seconds, 0 unknown
	closed    watch.Job                            // the attempt's time over the ticks whose paging gate was closed
	stops     int                                  // memory stops so far
	stopped   bool                                 // the last command was killed by a stop round
	peakKiB   int64                                // the last command's sampled peak
	why       func() (string, []engine.HolderInfo) // why the request is not admitted yet, and the live holds
	advised   bool                                 // the waiting line said once what to do about a tool's time limit
	lastStop  string                               // what the watchdog saw when it stopped the last command
	lastFloor bool                                 // the floor round stopped it
	reasons   []string                             // lastStop of every stop so far

	telemetry  bool                // the switch as the run read it at its start
	rec        *telemetry.Recorder // nil until the first enqueue succeeded, and with telemetry off
	trace      tracedStepper       // where the attempt's wait went; used only with rec
	snap       *engine.Report      // the last waiting line's snapshot; kept only with rec
	memMiB     int64               // the machine's memory, from the run's start
	oversized  bool                // the need can never fit beside another job
	needBefore int64               // a restart's need before its peak replaced it
}

// runCommand runs `coreslot run` and returns its exit status (PROTOCOL.md section 19).
func runCommand(args []string, io IO) int {
	loc := locate()
	o, err := parseRun(args, io)
	if errors.Is(err, errHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: %v\n", err)
		return exitUsage
	}
	if o.label == "" {
		cwd, _ := os.Getwd()
		o.label = autoLabel(o.argv, cwd)
	}
	var lookup needsLookup
	if loc.err == nil {
		lookup = newNeedsLookup(loc.cfgPath, func(format string, args ...any) {
			fmt.Fprintf(io.Stderr, "coreslot: "+format+"\n", args...)
		})
	}
	machine, _ := readMachine()
	needMiB, known, share, learned, typicalS, cpuS := readLog(loc, o.label)
	o.typicalS = typicalS
	o.resolveNeed(lookup, func() (int64, bool) { return needMiB, known }, machine.MemMiB)
	signal.Notify(runSignals, notIgnored(handledSignals...)...)
	defer signal.Stop(runSignals)
	installStops()
	r := &run{o: o, io: io, loc: loc, start: time.Now(), share: share, learned: learned, cpuS: cpuS, memMiB: machine.MemMiB}
	return r.decide()
}

func (r *run) notice(format string, args ...any) {
	fmt.Fprintf(r.io.Stderr, "coreslot: "+format+"\n", args...)
}

// refuse prints why the run refuses and returns the refusal code.
func (r *run) refuse(format string, args ...any) int {
	r.notice(format, args...)
	return r.o.refusalCode
}

// ordinary is a run that queues (PROTOCOL.md section 18 rule 3). A command a stop round killed
// queues again with its first arrival, until its third stop; --timeout bounds all the waits together.
func (r *run) ordinary() int {
	e, err := openEngine(r.loc, false)
	if err != nil {
		return r.failOpen(nil, err)
	}
	if r.o.restartMissing(r.o.sameGroup || terminalMode(r.io.Stdin)) { // before anything queues
		r.notice("%v", errRestartMissing)
		return exitUsage
	}
	req := r.o.request()
	bound, over, recording := e.Oversized(req.NeedMiB)
	if over {
		r.notice("needs %s, more than the %s the machine has above its reserve; starts only when no other coreslot job runs",
			spec.FormatMiB(req.NeedMiB), spec.FormatMiB(bound))
	}
	r.telemetry, r.oversized = recording, over
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if r.o.timeout > 0 { // one deadline from the run's start, restarts included
		var done context.CancelFunc
		ctx, done = context.WithTimeout(ctx, r.o.timeout)
		defer done()
	}
	for {
		g, code, ok := r.wait(ctx, cancel, e, req)
		if !ok {
			return code
		}
		if code = r.admitted(e, g); !r.stopped {
			return code
		}
		r.stops++
		r.reasons = append(r.reasons, r.lastStop)
		cmd := commandText(r.o.argv)
		switch {
		case r.o.noWait: // a restart waits for calm, which --no-wait does not
			return r.refuse("%s was stopped for memory (%s) and not restarted because of --no-wait%s", cmd, r.lastStop, r.floorAsk())
		case r.stops == maxStops:
			r.notice("gave up after %d memory stops (run %s)", maxStops, g.HoldName())
			for i, why := range r.reasons {
				r.notice("stop %d: %s", i+1, why)
			}
			r.notice("run %s alone, or narrow it%s", cmd, r.floorAsk())
			return r.o.refusalCode
		}
		ask := r.floorAsk()
		if ask != "" {
			ask += " before it comes back"
		}
		r.notice("memory stop %d of %d: %s was stopped (%s)%s; it was declared restartable (--restart=yes), so coreslot restarts it by itself; do not run it again",
			r.stops, maxStops, cmd, r.lastStop, ask)
		r.requeue(&req, g)
	}
}

// requeue makes req the request of the command g ran, coming back after a stop.
func (r *run) requeue(req *spec.Request, g *engine.Grant) {
	r.needBefore, r.o.needSource = req.NeedMiB, "restart_peak"
	req.ArrivalNS, req.Restart, req.NeedMiB = g.ArrivalNS(), true, restartNeed(r.peakKiB, r.o.need)
	r.start = time.Now()
}

// floorAsk is what the lines that report a floor stop ask of the agent; "" after a red stop.
func (r *run) floorAsk() string {
	if !r.lastFloor {
		return ""
	}
	return "; the whole machine ran out of memory (swap nearly full); free memory outside coreslot (close something)"
}

// commandText is the command for a message: at most 200 characters, escaped for a terminal.
func commandText(argv []string) string {
	s := []rune(text(strings.Join(argv, " ")))
	if len(s) > 200 {
		s = append(s[:200], '.', '.', '.')
	}
	return "`" + string(s) + "`"
}

// restartNeed is the need of a stopped command coming back: the larger of its peak before the
// kill and its need.
func restartNeed(peakKiB, needMiB int64) int64 { return max((peakKiB+1023)/1024, needMiB) }

// wait queues req and waits for its grant. ok is false when the run ends without one, with code
// its exit status.
func (r *run) wait(ctx context.Context, cancel context.CancelFunc, e *engine.Engine, req spec.Request) (g *engine.Grant, code int, ok bool) {
	r.why = func() (string, []engine.HolderInfo) {
		rep, err := e.Snapshot()
		if err != nil { // informative: a failure leaves it out
			r.snap = nil
			return "", nil
		}
		if r.rec != nil {
			r.snap = &rep
		}
		return waitReason(rep, req), rep.Holds
	}

	watch := watchSignals(cancel)

	enqCtx := ctx
	if r.o.noWait {
		var c context.CancelFunc
		enqCtx, c = context.WithTimeout(ctx, noWaitFor)
		defer c()
	}
	t, err := e.EnqueueContext(enqCtx, req)
	if err == nil {
		r.recordEnqueue(e, t, req)
		if r.o.noWait {
			g, err = r.tryOnce(ctx, t)
		} else {
			g, err = waitOneP(func() (*engine.Grant, error) {
				return waitLoop(ctx, r.stepperOf(t), newRealClock(), r.io.Stderr, r.waitingLine)
			})
		}
		if err != nil {
			_ = t.Withdraw()
		}
	}
	r.waited = time.Since(r.start)
	if sig := watch.end(); sig != nil {
		if g != nil {
			_ = g.Close()
		}
		if sig == syscall.SIGINT {
			r.notice("interrupted while waiting")
		}
		code, _ := classify(&interruptError{Sig: sig.(syscall.Signal)})
		return nil, code, false
	}
	if err != nil {
		return nil, r.notAdmitted(e, err, req.Restart), false
	}
	r.recordAdmitted(g)
	return g, 0, true
}

// notAdmitted handles a wait that ended without a grant. A restart never runs outside the budget.
func (r *run) notAdmitted(e *engine.Engine, err error, restart bool) int {
	switch {
	case errors.Is(err, engine.ErrCanceled):
		return r.refuse("the request was cancelled")
	case r.o.noWait && (errors.Is(err, errNotAtOnce) || errors.Is(err, context.DeadlineExceeded)):
		r.notice("not admitted at once (--no-wait)%s", r.holdersText(e))
		return r.o.refusalCode
	case errors.Is(err, context.DeadlineExceeded) && restart:
		return r.refuse("%s was stopped for memory (%s) and not restarted: the run's --timeout of %v ran out%s",
			commandText(r.o.argv), r.lastStop, r.o.timeout, r.floorAsk())
	case errors.Is(err, context.DeadlineExceeded):
		if r.o.onTimeout == "run" {
			return r.timeoutRun(e)
		}
		return r.refuse("not admitted within %v (--timeout); refusing", r.o.timeout)
	}
	return r.failOpen(e, err)
}
