//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// cancelWait is how long cancel waits to enter the admission section before it refuses: a client
// stopped inside the section would otherwise keep it waiting for as long as it stays stopped.
const cancelWait = time.Second

// cancelCommand is `coreslot cancel`: it publishes a cancel request (PROTOCOL.md section 13.1) for
// a waiting ticket, named by the id `status` shows or by its label. A label that matches several
// tickets, no waiting ticket, a state directory that cannot be used or whose format this client
// does not implement, and an admission section held for longer than cancelWait are refusals
// (exit 75, not --refusal-code, which belongs to `run`). The request is a file the ticket's owner
// finds on its next poll; nothing is signaled and a grant is never revoked.
func cancelCommand(args []string, io IO) int {
	fs, label := cancelFlags()
	id, byLabel, code := ticketArgs(fs, "cancel", cancelUsage, args, io)
	if code >= 0 {
		return code
	}
	c := ticketCommand{verb: "cancel", noTicket: "no waiting ticket %s (it was admitted, withdrawn or removed)",
		nothing: "no cancel request was published", publish: (*engine.Engine).CancelContext}
	if _, id, code = c.request(id, byLabel, *label, io); code >= 0 {
		return code
	}
	fmt.Fprintf(io.Stdout, "coreslot: cancel requested for ticket %s\n", id)
	return 0
}

// ticketArgs parses the arguments of cancel and start, a ticket id or --label: it returns the id
// (zero with --label), whether the label names it, and -1, or the exit status after saying why not.
func ticketArgs(fs *flag.FlagSet, verb, usage string, args []string, io IO) (wire.ID, bool, int) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(io.Stdout, usage)
			return wire.ID{}, false, 0
		}
		fmt.Fprintf(io.Stderr, "coreslot: %s\n%s\n", text(err.Error()), usage)
		return wire.ID{}, false, exitUsage
	}
	byLabel := false
	fs.Visit(func(f *flag.Flag) { byLabel = byLabel || f.Name == "label" })
	if byLabel == (fs.NArg() > 0) || fs.NArg() > 1 {
		fmt.Fprintf(io.Stderr, "coreslot: %s takes a ticket id or --label\n%s\n", verb, usage)
		return wire.ID{}, false, exitUsage
	}
	if byLabel {
		return wire.ID{}, true, -1
	}
	if id, ok := ticketID(fs.Arg(0), io); ok {
		return id, false, -1
	}
	return wire.ID{}, false, exitUsage
}

// ticketID parses s as a ticket id, and says so when it is not one.
func ticketID(s string, io IO) (wire.ID, bool) {
	id, ok := wire.ParseName(s)
	if !ok {
		fmt.Fprintf(io.Stderr, "coreslot: %q is not a ticket id (the id column of `coreslot status`)\n", s)
	}
	return id, ok
}

// ticketCommand is what cancel, start and move share once their arguments are read: the request
// for one waiting ticket, published within cancelWait.
type ticketCommand struct {
	verb     string // cancel, start or move
	noTicket string // why a ticket that is not waiting is refused, with %s for its id
	nothing  string // what a section held too long left unpublished
	// publish publishes the request for id inside the section, entered within ctx.
	publish func(e *engine.Engine, ctx context.Context, id wire.ID) error
	// named, when set, checks the id once it is known, and returns -1 to go on or an exit status.
	named func(id wire.ID) int
}

// request opens the engine and publishes the request for id, or for the one waiting ticket labelled
// label when byLabel. It returns the engine, the id and -1 once published, or the exit status after
// saying why not: no state directory, a label that names none or several, a ticket that is not
// waiting and a section held for longer than cancelWait are refusals (exit 75).
func (c ticketCommand) request(id wire.ID, byLabel bool, label string, io IO) (*engine.Engine, wire.ID, int) {
	e, err := openEngine(locate(), true)
	if err != nil {
		return nil, id, commandFailed(c.verb, err, io)
	}
	if !e.Store().Exists() { // nothing has run, so nothing waits; do not create the directory for it
		fmt.Fprintf(io.Stderr, "coreslot: %s: no state directory yet (nothing has run), so no ticket is waiting\n", c.verb)
		return nil, id, exitRefusal
	}
	if byLabel {
		var code int
		if id, code = soleWaiting(e, c.verb, label, io); code >= 0 {
			return nil, id, code
		}
	}
	if c.named != nil {
		if code := c.named(id); code >= 0 {
			return nil, id, code
		}
	}
	ctx, stop := context.WithTimeout(context.Background(), cancelWait)
	defer stop()
	// The publication gives up entering the section when ctx ends, publishing nothing. Only that is
	// a busy section: an error after a timely entry is reported as itself.
	var busy *engine.SectionBusyError
	switch err := c.publish(e, ctx, id); {
	case err == nil:
		return e, id, -1
	case errors.Is(err, engine.ErrNoTicket):
		fmt.Fprintf(io.Stderr, "coreslot: %s: "+c.noTicket+"\n", c.verb, id)
	case errors.As(err, &busy), errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(io.Stderr, "coreslot: %s refused: %s; %s\n", c.verb, sectionBusyText(e, cancelWait), c.nothing)
	default:
		return nil, id, commandFailed(c.verb, err, io)
	}
	return nil, id, exitRefusal
}

// soleWaiting is the id of the one waiting ticket labelled label, for verb, and -1;
// or, when none or several are, the exit status after it says so. A snapshot that fails is
// reported as verb's failure.
func soleWaiting(e *engine.Engine, verb, label string, io IO) (wire.ID, int) {
	rep, err := e.Snapshot()
	if err != nil {
		return wire.ID{}, commandFailed(verb, err, io)
	}
	var ids []string
	for _, t := range rep.Tickets {
		if t.Label == label && !t.Dead {
			ids = append(ids, t.ID)
		}
	}
	slices.Sort(ids)
	switch len(ids) {
	case 0:
		fmt.Fprintf(io.Stderr, "coreslot: %s: no waiting ticket has the label %s\n", verb, text(label))
		return wire.ID{}, exitRefusal
	case 1:
		id, _ := wire.ParseID(ids[0])
		return id, -1
	}
	fmt.Fprintf(io.Stderr, "coreslot: %s: %d waiting tickets have the label %s; %s one by id: %s\n",
		verb, len(ids), text(label), verb, strings.Join(ids, ", "))
	return wire.ID{}, exitRefusal
}

// sectionBusyText says that the admission section stayed held for waited and, from admit.lock's
// informational body, who holds it now.
func sectionBusyText(e *engine.Engine, waited time.Duration) string {
	s := fmt.Sprintf("the admission section stayed held for %s", duration(waited))
	if rep, err := e.Snapshot(); err == nil && rep.Section.Held {
		s += "; it is " + sectionHolder(rep.Section, store.NowNS())
	}
	return s
}

// commandFailed reports an error that kept verb from publishing its file. A refusal
// of the engine and a state error (PROTOCOL.md section 14: the directory or its configuration
// cannot be used) are refusals, exit 75; anything else is an internal error, exit 70.
func commandFailed(verb string, err error, io IO) int {
	if code, stateErr := classify(err); stateErr || code == exitRefusal {
		fmt.Fprintf(io.Stderr, "coreslot: %s refused: %s\n", verb, text(refusalText(err)))
		return exitRefusal
	}
	fmt.Fprintf(io.Stderr, "coreslot: %s: %s\n", verb, text(err.Error()))
	return exitInternal
}

// refusalText is err's message without the "coreslot: refused: " that the engine's refusals
// start with, for a caller that prints its own prefix.
func refusalText(err error) string {
	return strings.TrimPrefix(err.Error(), engine.ErrRefused.Error()+": ")
}
