//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
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
	env       []string           // the command's environment before its grant: the inherited context dropped
	pipeNoted bool               // the waiting line said once whether stdin is a pipe
	freeWait  *freeMeter         // how long it waited while its need was free
	normal    map[string]float64 // every label's normal share, from the run log
	stops     int                // memory stops so far
	stopped   bool               // the last command was killed by a stop round
	peakKiB   int64              // the last command's sampled peak
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
// queues again with its first arrival, until its third stop; --timeout covers every wait.
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
	if r.o.timeout > 0 {
		var c context.CancelFunc
		ctx, c = context.WithTimeout(ctx, r.o.timeout)
		defer c()
	}
	for {
		g, code, ok := r.wait(ctx, cancel, e, req)
		if !ok {
			return code
		}
		if code = r.admitted(e, g); !r.stopped {
			return code
		}
		if r.stops++; r.stops == maxStops {
			r.notice("gave up after %d memory stops (run %s)", maxStops, g.HoldName())
			return r.o.refusalCode
		}
		r.notice("memory stop %d of %d: the command was killed to free memory; coreslot restarts it by itself; do not run it again",
			r.stops, maxStops)
		req.ArrivalNS, req.Restart, req.NeedMiB = g.ArrivalNS(), true, restartNeed(r.peakKiB, r.o.need)
		r.start, r.startNS = time.Now(), time.Now().UnixNano()
	}
}

// restartNeed is the need of a stopped command coming back: the larger of its peak before the
// kill and its need.
func restartNeed(peakKiB, needMiB int64) int64 { return max((peakKiB+1023)/1024, needMiB) }

// wait queues req and waits for its grant. ok is false when the run ends without one, with code
// its exit status.
func (r *run) wait(ctx context.Context, cancel context.CancelFunc, e *engine.Engine, req spec.Request) (g *engine.Grant, code int, ok bool) {
	r.freeWait = &freeMeter{needMiB: req.NeedMiB, headroom: memsample.Headroom}
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
				return waitLoop(ctx, ticketStepper{t}, newRealClock(), r.io.Stderr, r.waitingLine, r.freeWait)
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
		return nil, r.notAdmitted(e, err), false
	}
	return g, 0, true
}

// notAdmitted handles a wait that ended without a grant.
func (r *run) notAdmitted(e *engine.Engine, err error) int {
	switch {
	case errors.Is(err, engine.ErrCanceled):
		return r.refuse("the request was cancelled")
	case r.o.noWait && (errors.Is(err, errNotAtOnce) || errors.Is(err, context.DeadlineExceeded)):
		r.notice("not admitted at once (--no-wait)%s", r.holdersText(e))
		return r.o.refusalCode
	case errors.Is(err, context.DeadlineExceeded):
		if r.o.onTimeout == "run" {
			return r.timeoutRun(e)
		}
		return r.refuse("not admitted within %v (--timeout); refusing", r.o.timeout)
	}
	return r.failOpen(e, err)
}
