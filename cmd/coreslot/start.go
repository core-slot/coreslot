//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/wire"
)

// startAnswerWait is how long start waits for the ticket's run to start; tests shorten it.
var startAnswerWait = 8 * time.Second

// startCommand is `coreslot start`: it publishes a start request (PROTOCOL.md section 7.16) for a
// waiting ticket, named as cancel names one, and waits outside the section for the ticket's own run
// to admit itself with a forced hold. The refusals are cancel's (exit 75). A run that does not
// answer in time is not one: exit 0, as with --no-wait, which does not wait.
func startCommand(args []string, io IO) int {
	fs, label, noWait := startFlags()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(io.Stdout, startUsage)
			return 0
		}
		fmt.Fprintf(io.Stderr, "coreslot: %s\n%s\n", text(err.Error()), startUsage)
		return exitUsage
	}
	byLabel := false
	fs.Visit(func(f *flag.Flag) { byLabel = byLabel || f.Name == "label" })
	if byLabel == (fs.NArg() > 0) || fs.NArg() > 1 {
		fmt.Fprintf(io.Stderr, "coreslot: start takes a ticket id or --label\n%s\n", startUsage)
		return exitUsage
	}
	var id wire.ID
	if !byLabel {
		var ok bool
		if id, ok = wire.ParseName(fs.Arg(0)); !ok {
			fmt.Fprintf(io.Stderr, "coreslot: %q is not a ticket id (the id column of `coreslot status`)\n", fs.Arg(0))
			return exitUsage
		}
	}

	e, err := openEngine(locate(), true)
	if err != nil {
		return commandFailed("start", err, io)
	}
	if !e.Store().Exists() {
		fmt.Fprintln(io.Stderr, "coreslot: start: no state directory yet (nothing has run), so no ticket is waiting")
		return exitRefusal
	}
	if byLabel {
		var code int
		if id, code = soleWaiting(e, "start", *label, io); code >= 0 {
			return code
		}
	}

	ctx, stop := context.WithTimeout(context.Background(), cancelWait)
	defer stop()
	var busy *engine.SectionBusyError
	switch err := e.StartContext(ctx, id); {
	case err == nil:
	case errors.Is(err, engine.ErrNoTicket):
		fmt.Fprintf(io.Stderr, "coreslot: start: no waiting ticket %s (it was admitted, withdrawn or removed)\n", id)
		return exitRefusal
	case errors.As(err, &busy), errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(io.Stderr, "coreslot: start refused: %s; no start request was published\n", sectionBusyText(e, cancelWait))
		return exitRefusal
	default:
		return commandFailed("start", err, io)
	}
	if *noWait {
		fmt.Fprintf(io.Stdout, "coreslot: start requested for ticket %s\n", id)
		return 0
	}
	h, started, gone := awaitStart(e, id)
	switch {
	case started:
		fmt.Fprintf(io.Stdout, "coreslot: %s started now, out of turn; its full need (%s) still counts\n", labelText(h.Label), spec.FormatMiB(h.NeedMiB))
	case gone:
		fmt.Fprintf(io.Stdout, "coreslot: ticket %s left the queue and no forced hold of its run is live (cancelled, withdrawn, or already finished)\n", id)
	default:
		fmt.Fprintf(io.Stdout, "coreslot: ticket %s: asked; the run has not answered yet. A run started by an older coreslot never will; see coreslot status\n", id)
	}
	return 0
}

// awaitStart polls for up to startAnswerWait until the ticket id has left the queue, and returns
// its run's forced hold: started when one shows, gone when the ticket left without one. The
// snapshot lists the queue before the holds, and the hold is published before the ticket goes.
func awaitStart(e *engine.Engine, id wire.ID) (h engine.HolderInfo, started, gone bool) {
	deadline := time.Now().Add(startAnswerWait)
	for {
		if r, err := e.Snapshot(); err == nil && !waitingIn(r, id) {
			for _, h := range r.Holds {
				if h.Forced && h.PID == id.PID {
					return h, true, false
				}
			}
			return h, false, true
		}
		if !time.Now().Before(deadline) {
			return h, false, false
		}
		time.Sleep(min(trimAnswerPoll, time.Until(deadline)))
	}
}

// waitingIn reports whether rep lists the ticket id as live.
func waitingIn(rep engine.Report, id wire.ID) bool {
	for _, t := range rep.Tickets {
		if t.ID == id.String() && !t.Dead {
			return true
		}
	}
	return false
}
