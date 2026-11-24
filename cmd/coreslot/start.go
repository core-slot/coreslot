//go:build unix

package main

import (
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
	id, byLabel, code := ticketArgs(fs, "start", startUsage, args, io)
	if code >= 0 {
		return code
	}
	c := ticketCommand{verb: "start", noTicket: "no waiting ticket %s (it was admitted, withdrawn or removed)",
		nothing: "no start request was published", publish: (*engine.Engine).StartContext}
	e, id, code := c.request(id, byLabel, *label, io)
	if code >= 0 {
		return code
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
		if r, err := e.Snapshot(); err == nil && !waitingTickets(r)[id.String()] {
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
