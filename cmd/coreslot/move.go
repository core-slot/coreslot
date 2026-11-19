//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/wire"
)

// moveCommand is `coreslot move`: it changes a waiting ticket's place in the queue by publishing
// its order override (PROTOCOL.md section 13.3), never touching the ticket or its run. The ticket
// is named by its id or, with --label, by the label of the one waiting ticket that has it; exactly
// one of --first, --last, --before and --after says where it goes, and the flags may come before or
// after the id. A ticket or a reference that is not waiting, a label that matches several, a state
// directory that cannot be used or whose format this client does not implement, and an admission
// section held for longer than cancelWait are refusals (exit 75); a reference that is the ticket
// itself is a usage error.
func moveCommand(args []string, io IO) int {
	fs, label, _, last, before, after := moveFlags()
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprintln(io.Stdout, moveUsage)
				return 0
			}
			fmt.Fprintf(io.Stderr, "coreslot: %s\n%s\n", text(err.Error()), moveUsage)
			return exitUsage
		}
		if fs.NArg() == 0 {
			break
		}
		positional, args = append(positional, fs.Arg(0)), fs.Args()[1:]
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	places := 0
	for _, name := range []string{"first", "last", "before", "after"} {
		if set[name] {
			places++
		}
	}
	if set["label"] == (len(positional) > 0) || len(positional) > 1 || places != 1 {
		fmt.Fprintf(io.Stderr, "coreslot: move takes a ticket id or --label, and one of --first, --last, --before, --after\n%s\n", moveUsage)
		return exitUsage
	}
	place, refText := engine.PlaceFirst, ""
	switch {
	case *last:
		place = engine.PlaceLast
	case set["before"]:
		place, refText = engine.PlaceBefore, *before
	case set["after"]:
		place, refText = engine.PlaceAfter, *after
	}
	var id, ref wire.ID
	for _, p := range []struct {
		s  string
		id *wire.ID
	}{{refText, &ref}, {firstOf(positional), &id}} {
		if p.s == "" {
			continue
		}
		var ok bool
		if *p.id, ok = wire.ParseName(p.s); !ok {
			fmt.Fprintf(io.Stderr, "coreslot: %q is not a ticket id (the id column of `coreslot status`)\n", p.s)
			return exitUsage
		}
	}

	e, err := openEngine(locate(), true)
	if err != nil {
		return commandFailed("move", err, io)
	}
	if !e.Store().Exists() {
		fmt.Fprintln(io.Stderr, "coreslot: move: no state directory yet (nothing has run), so no ticket is waiting")
		return exitRefusal
	}
	if set["label"] {
		var code int
		if id, code = soleWaiting(e, "move", *label, io); code >= 0 {
			return code
		}
	}
	if refText != "" && ref == id {
		fmt.Fprintf(io.Stderr, "coreslot: move: a ticket cannot be moved next to itself\n%s\n", moveUsage)
		return exitUsage
	}

	ctx, stop := context.WithTimeout(context.Background(), cancelWait)
	defer stop()
	var busy *engine.SectionBusyError
	switch _, err := e.MoveContext(ctx, id, place, ref); {
	case err == nil:
		fmt.Fprintf(io.Stdout, "coreslot: moved ticket %s\n", id)
		return 0
	case errors.Is(err, engine.ErrNoTicket):
		fmt.Fprintf(io.Stderr, "coreslot: move: ticket %s or the one it is placed by is not waiting (admitted, withdrawn or removed)\n", id)
		return exitRefusal
	case errors.As(err, &busy), errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(io.Stderr, "coreslot: move refused: %s; nothing was moved\n", sectionBusyText(e, cancelWait))
		return exitRefusal
	default:
		return commandFailed("move", err, io)
	}
}

// firstOf is the first of args, "" when there is none.
func firstOf(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
