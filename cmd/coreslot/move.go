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
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(io.Stdout, moveUsage)
			return 0
		}
		fmt.Fprintf(io.Stderr, "coreslot: %s\n%s\n", text(err.Error()), moveUsage)
		return exitUsage
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
		if *p.id, ok = ticketID(p.s, io); !ok {
			return exitUsage
		}
	}

	c := ticketCommand{verb: "move", noTicket: "ticket %s or the one it is placed by is not waiting (admitted, withdrawn or removed)",
		nothing: "nothing was moved", named: func(id wire.ID) int {
			if refText != "" && ref == id {
				fmt.Fprintf(io.Stderr, "coreslot: move: a ticket cannot be moved next to itself\n%s\n", moveUsage)
				return exitUsage
			}
			return -1
		}, publish: func(e *engine.Engine, ctx context.Context, id wire.ID) error {
			_, err := e.MoveContext(ctx, id, place, ref)
			return err
		}}
	_, id, code := c.request(id, set["label"], *label, io)
	if code >= 0 {
		return code
	}
	fmt.Fprintf(io.Stdout, "coreslot: moved ticket %s\n", id)
	return 0
}

// firstOf is the first of args, "" when there is none.
func firstOf(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
