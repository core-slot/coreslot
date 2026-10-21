//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/needs"
	"github.com/core-slot/coreslot/internal/spec"
)

// Timing of a waiting run (PROTOCOL.md section 22).
const (
	waitLineEvery = 30 * time.Second // the waiting line
	noWaitFor     = time.Second      // --no-wait: how long the section is tried
)

// run is one `coreslot run`.
type run struct {
	o         runOptions
	io        IO
	loc       location // the state directory and configuration, located once
	startNS   int64    // wall clock at the run's start
	start     time.Time
	load      float64
	waited    time.Duration
	env       []string     // the command's environment before its grant: the inherited context dropped
	nofile    *nofileProbe // learns the descriptor limit the run was started with (rlimit_unix.go)
	pipeNoted bool         // the waiting line said once whether stdin is a pipe
	freeWait  *freeMeter   // set when the request names mem: how long it waited while that was free
}

// runCommand runs `coreslot run` and returns its exit status (PROTOCOL.md section 19).
func runCommand(args []string, io IO) int {
	loc := locate()
	var lookup func(string) (needs.Entry, bool)
	if loc.err == nil {
		lookup = newNeedsLookup(loc.cfgPath, func(format string, args ...any) {
			fmt.Fprintf(io.Stderr, "coreslot: "+format+"\n", args...)
		})
	}
	o, err := parseRun(args, io, lookup)
	if errors.Is(err, errHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: %v\n", err)
		return exitUsage
	}
	signal.Notify(runSignals, notIgnored(handledSignals...)...)
	defer signal.Stop(runSignals)
	installStops()
	r := &run{o: o, io: io, loc: loc, startNS: time.Now().UnixNano(), start: time.Now(), load: load1()}
	defer func() { r.nofile.stop() }()
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

// priority is the request's priority: --priority, or else the configuration's [priorities]
// entry of the longest prefix of the label, or 0. A configuration that cannot be read is a
// state error, under --priority too.
func (r *run) priority(e *engine.Engine) (int, error) {
	cfg, err := e.Config()
	if err != nil {
		return 0, err
	}
	if r.o.prioritySet {
		return r.o.priority, nil
	}
	p, _ := cfg.PriorityFor(r.o.label)
	return p, nil
}

// ordinary is a run that queues (PROTOCOL.md section 18 rule 3).
func (r *run) ordinary() int {
	e, err := openEngine(r.loc, false)
	if err != nil {
		return r.failOpen(nil, err)
	}
	if r.o.light {
		return r.lightRun(e)
	}
	prio, err := r.priority(e)
	if err != nil {
		return r.failOpen(e, err)
	}
	req := r.o.request(prio)
	if mem, ok := req.Need["mem"]; ok {
		r.freeWait = &freeMeter{needMiB: int64(mem.Min) * spec.MemTokenMiB, headroom: memsample.Headroom}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if r.o.timeout > 0 {
		var c context.CancelFunc
		ctx, c = context.WithTimeout(ctx, r.o.timeout)
		defer c()
	}
	watch := watchSignals(cancel)

	enqCtx := ctx
	if r.o.noWait {
		var c context.CancelFunc
		enqCtx, c = context.WithTimeout(ctx, noWaitFor)
		defer c()
	}
	t, err := e.EnqueueContext(enqCtx, req)
	var g *engine.Grant
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
		return code
	}
	if err != nil {
		return r.notAdmitted(e, t, err)
	}
	return r.admitted(e, g)
}

// notAdmitted handles a wait that ended without a grant.
func (r *run) notAdmitted(e *engine.Engine, t *engine.Ticket, err error) int {
	switch {
	case errors.Is(err, engine.ErrCanceled):
		return r.refuse("the request was cancelled")
	case r.o.noWait && (errors.Is(err, errNotAtOnce) || errors.Is(err, context.DeadlineExceeded)):
		r.notice("not admitted at once (--no-wait)%s", r.holdersText(e))
		return r.o.refusalCode
	case errors.Is(err, context.DeadlineExceeded):
		if r.o.onTimeout == "run" {
			return r.timeoutRun(e, t)
		}
		return r.refuse("not admitted within %v (--timeout); refusing", r.o.timeout)
	}
	return r.failOpen(e, err)
}
