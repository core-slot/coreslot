//go:build unix

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"

	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

const cancelUsage = `usage: coreslot cancel <ticket-id>
       coreslot cancel --label LABEL`

// cancelCommand is `coreslot cancel`: it publishes a cancel
// request for a waiting ticket, named by the id `status` shows or by its label. A label that
// matches several tickets, no waiting ticket, or a state directory format this client does not
// implement is a refusal (exit 75, not --refusal-code, which belongs to `run`). The request is
// a file the ticket's owner finds on its next poll; nothing is signalled and a grant is never
// revoked.
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
		fmt.Fprintf(io.Stderr, "coreslot: cancel: %s\n", text(err.Error()))
		return exitInternal
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
			fmt.Fprintf(io.Stderr, "coreslot: cancel: %s\n", text(err.Error()))
			return exitInternal
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

	switch err := inSection(func() error { return e.Cancel(id) }); {
	case err == nil:
		fmt.Fprintf(io.Stdout, "coreslot: cancel requested for ticket %s\n", id)
		return 0
	case errors.Is(err, engine.ErrNoTicket):
		fmt.Fprintf(io.Stderr, "coreslot: cancel: no waiting ticket %s (it was admitted, withdrawn or removed)\n", id)
		return exitRefusal
	case errors.Is(err, engine.ErrRefused):
		fmt.Fprintf(io.Stderr, "coreslot: cancel: %s\n", text(err.Error()))
		return exitRefusal
	default:
		fmt.Fprintf(io.Stderr, "coreslot: cancel: %s\n", text(err.Error()))
		return exitInternal
	}
}
