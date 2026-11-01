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
)

// Timing of a waiting run (PROTOCOL.md section 22).
const (
	waitLineEvery = 30 * time.Second // the waiting line
	noWaitFor     = time.Second      // --no-wait: how long the section is tried
)

// maxStops is the memory stops after which a run gives up.
const maxStops = 3

// readLog reads the run log once for a run: the need it gives label, the p90 peak of its last
// counted runs (ok false when unknown), and every label's normal share of system time.
func readLog(loc location, label string) (needMiB int64, ok bool, normal map[string]float64) {
	normal = map[string]float64{}
	if loc.err != nil {
		return 0, false, normal
	}
	st, err := store.OpenReadOnly(loc.dir)
	if err != nil || !st.Exists() {
		return 0, false, normal
	}
	runs, _, err := st.ReadRuns()
	if err != nil {
		return 0, false, normal
	}
	for _, s := range runstats.Summarize(runs) {
		if share, ok := runstats.NormalShare(s.Window); ok {
			normal[s.Label] = share
		}
	}
	needMiB, ok = runstats.NeedMiB(runstats.Window(runs, label, runstats.WindowRuns))
	return needMiB, ok, normal
}

// run is one `coreslot run`.
type run struct {
	o         runOptions
	io        IO
	loc       location // the state directory and configuration, located once
	startNS   int64    // wall clock at the run's start
	start     time.Time
	waited    time.Duration
	env       []string                             // the command's environment before its grant: the inherited context dropped
	pipeNoted bool                                 // the waiting line said once whether stdin is a pipe
	normal    map[string]float64                   // every label's normal share, from the run log
	stops     int                                  // memory stops so far
	stopped   bool                                 // the last command was killed by a stop round
	peakKiB   int64                                // the last command's sampled peak
	why       func() (string, []engine.HolderInfo) // why the request is not admitted yet, and the live holds
	advised   bool                                 // the waiting line said once what to do about a tool's time limit
	lastStop  string                               // what the watchdog saw when it stopped the last command
	reasons   []string                             // lastStop of every stop so far
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
	learned, known, normal := readLog(loc, o.label)
	o.resolveNeed(lookup, func() (int64, bool) { return learned, known }, machine.MemMiB)
	signal.Notify(runSignals, notIgnored(handledSignals...)...)
	defer signal.Stop(runSignals)
	installStops()
	r := &run{o: o, io: io, loc: loc, startNS: time.Now().UnixNano(), start: time.Now(), normal: normal}
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
// queues again with its first arrival, until its third stop; --timeout bounds each wait.
func (r *run) ordinary() int {
	e, err := openEngine(r.loc, false)
	if err != nil {
		return r.failOpen(nil, err)
	}
	req := r.o.request()
	if total, over := e.Oversized(req.NeedMiB); over {
		r.notice("needs %.1f GiB of %.1f GiB; runs only alone", float64(req.NeedMiB)/1024, float64(total)/1024)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
			return r.refuse("%s was stopped for memory (%s) and not restarted because of --no-wait", cmd, r.lastStop)
		case r.stops == maxStops:
			r.notice("gave up after %d memory stops (run %s)", maxStops, g.HoldName())
			for i, why := range r.reasons {
				r.notice("stop %d: %s", i+1, why)
			}
			r.notice("run %s alone, or narrow it", cmd)
			return r.o.refusalCode
		}
		r.notice("memory stop %d of %d: %s was stopped (%s); coreslot restarts it by itself; do not run it again",
			r.stops, maxStops, cmd, r.lastStop)
		req.ArrivalNS, req.Restart, req.NeedMiB = g.ArrivalNS(), true, restartNeed(r.peakKiB, r.o.need)
		r.start, r.startNS = time.Now(), time.Now().UnixNano()
	}
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
	if r.o.timeout > 0 {
		var c context.CancelFunc
		ctx, c = context.WithTimeout(ctx, r.o.timeout)
		defer c()
	}
	r.why = func() (string, []engine.HolderInfo) {
		rep, err := e.Snapshot()
		if err != nil { // informative: a failure leaves it out
			return "", nil
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
		if r.o.noWait {
			g, err = r.tryOnce(ctx, t)
		} else {
			g, err = waitOneP(func() (*engine.Grant, error) {
				return waitLoop(ctx, ticketStepper{t}, newRealClock(), r.io.Stderr, r.waitingLine)
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
		return r.refuse("%s was stopped for memory (%s) and not restarted within %v (--timeout)",
			commandText(r.o.argv), r.lastStop, r.o.timeout)
	case errors.Is(err, context.DeadlineExceeded):
		if r.o.onTimeout == "run" {
			return r.timeoutRun(e)
		}
		return r.refuse("not admitted within %v (--timeout); refusing", r.o.timeout)
	}
	return r.failOpen(e, err)
}
