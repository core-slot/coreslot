//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

const cancelUsage = `usage: coreslot cancel <ticket-id>
       coreslot cancel --label LABEL`

// cancelWait is how long cancel waits to enter the admission section before it refuses: a client
// stopped inside the section would otherwise keep it waiting for as long as it stays stopped.
const cancelWait = time.Second

// cancelCommand is `coreslot cancel`: it publishes a cancel request (PROTOCOL.md section 13.1) for
// a waiting ticket, named by the id `status` shows or by its label. A label that matches several
// tickets, no waiting ticket, a state directory that cannot be used or whose format this client
// does not implement, and an admission section held for longer than cancelWait are refusals
// (exit 75, not --refusal-code, which belongs to `run`). The request is a file the ticket's owner
// finds on its next poll; nothing is signalled and a grant is never revoked.
func cancelCommand(args []string, io IO) int {
	fs := flag.NewFlagSet("coreslot cancel", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	label := fs.String("label", "", "cancel the one waiting ticket with this label")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(io.Stdout, cancelUsage)
			return 0
		}
		fmt.Fprintf(io.Stderr, "coreslot: %s\n%s\n", text(err.Error()), cancelUsage)
		return exitUsage
	}
	byLabel := false
	fs.Visit(func(f *flag.Flag) { byLabel = byLabel || f.Name == "label" })
	if byLabel == (fs.NArg() > 0) || fs.NArg() > 1 {
		fmt.Fprintf(io.Stderr, "coreslot: cancel takes a ticket id or --label\n%s\n", cancelUsage)
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

	e, _, err := openEngineReadOnly()
	if err != nil {
		return cancelFailed(err, io)
	}
	if !e.Store().Exists() { // nothing has run, so nothing waits; do not create the directory for it
		fmt.Fprintln(io.Stderr, "coreslot: cancel: no state directory yet (nothing has run), so no ticket is waiting")
		return exitRefusal
	}
	if byLabel {
		var rep engine.Report
		if err := inSection(func() (err error) { // Snapshot probes admit.lock
			rep, err = e.Snapshot()
			return err
		}); err != nil {
			return cancelFailed(err, io)
		}
		var ids []string
		for _, t := range rep.Tickets {
			if t.Label == *label && !t.Dead {
				ids = append(ids, t.ID)
			}
		}
		slices.Sort(ids)
		switch len(ids) {
		case 0:
			fmt.Fprintf(io.Stderr, "coreslot: cancel: no waiting ticket has the label %s\n", text(*label))
			return exitRefusal
		case 1:
			id, _ = wire.ParseID(ids[0])
		default:
			fmt.Fprintf(io.Stderr, "coreslot: cancel: %d waiting tickets have the label %s; cancel one by id: %s\n",
				len(ids), text(*label), strings.Join(ids, ", "))
			return exitRefusal
		}
	}

	ctx, stop := context.WithTimeout(context.Background(), cancelWait)
	defer stop()
	// An engine with CancelContext gives up entering the section when ctx ends; one without it
	// waits for the section.
	switch err := inSection(func() error {
		if c, ok := any(e).(contextCanceller); ok {
			return c.CancelContext(ctx, id)
		}
		return e.Cancel(id)
	}); {
	case err == nil:
		fmt.Fprintf(io.Stdout, "coreslot: cancel requested for ticket %s\n", id)
		return 0
	case errors.Is(err, engine.ErrNoTicket):
		fmt.Fprintf(io.Stderr, "coreslot: cancel: no waiting ticket %s (it was admitted, withdrawn or removed)\n", id)
		return exitRefusal
	case errors.Is(err, context.DeadlineExceeded), ctx.Err() != nil && !errors.Is(err, engine.ErrRefused):
		fmt.Fprintf(io.Stderr, "coreslot: cancel refused: %s; no cancel request was published\n", sectionBusyText(e, cancelWait))
		return exitRefusal
	default:
		return cancelFailed(err, io)
	}
}

// contextCanceller is an engine whose Cancel gives up when ctx ends before it has entered the
// admission section.
type contextCanceller interface {
	CancelContext(ctx context.Context, id wire.ID) error
}

// sectionBusyText says that the admission section stayed held for waited and, from admit.lock's
// informational body, who holds it now.
func sectionBusyText(e *engine.Engine, waited time.Duration) string {
	s := fmt.Sprintf("the admission section stayed held for %s", duration(waited))
	var rep engine.Report
	if err := inSection(func() (err error) { // Snapshot probes admit.lock
		rep, err = e.Snapshot()
		return err
	}); err == nil && rep.Section.Held {
		s += "; it is " + sectionHolder(rep.Section, store.NowNS())
	}
	return s
}

// cancelFailed reports an error that kept cancel from publishing its request. A refusal of the
// engine and a state error (PROTOCOL.md section 14: the directory or its configuration cannot be
// used) are refusals, exit 75; anything else is an internal error, exit 70.
func cancelFailed(err error, io IO) int {
	if _, stateErr := classify(err); stateErr || errors.Is(err, engine.ErrRefused) {
		fmt.Fprintf(io.Stderr, "coreslot: cancel refused: %s\n", text(refusalText(err)))
		return exitRefusal
	}
	fmt.Fprintf(io.Stderr, "coreslot: cancel: %s\n", text(err.Error()))
	return exitInternal
}

// refusalText is err's message without the "coreslot: refused: " that the engine's refusals
// start with, for a caller that prints its own prefix.
func refusalText(err error) string {
	return strings.TrimPrefix(err.Error(), engine.ErrRefused.Error()+": ")
}
