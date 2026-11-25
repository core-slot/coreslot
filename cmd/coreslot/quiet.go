//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// quietCommand is `coreslot quiet`: it marks a waiting or running job so that, while it runs, no
// other job is admitted (PROTOCOL.md section 7.18); --off removes the mark.
func quietCommand(args []string, io IO) int {
	fs, off := quietFlags()
	return markCommand("quiet", quietUsage, store.QuietDir, fs, off, args, io,
		"quiet window set for %s: while it runs, no other job starts", "quiet window removed from %s")
}

// noStopCommand is `coreslot no-stop`: it marks a waiting or running job so that the watchdog never
// stops it (PROTOCOL.md section 7.18); --off removes the mark.
func noStopCommand(args []string, io IO) int {
	fs, off := noStopFlags()
	return markCommand("no-stop", noStopUsage, store.NoStopDir, fs, off, args, io,
		"%s marked do-not-stop: the watchdog will not stop it", "do-not-stop removed from %s")
}

// markCommand marks, or with off unmarks, the job named by the id `status` shows on a ticket's or a
// hold's line. Exit 0 once marked (or already) or unmarked; no live ticket or hold to mark, no mark
// to remove, a state directory that cannot be used or whose format this client does not implement,
// and an admission section held for longer than cancelWait are refusals (exit 75).
func markCommand(verb, usage, dir string, fs *flag.FlagSet, off *bool, args []string, io IO, marked, unmarked string) int {
	pos, err := parseInterleaved(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(io.Stdout, usage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: %s\n%s\n", text(err.Error()), usage)
		return exitUsage
	}
	if len(pos) != 1 {
		fmt.Fprintf(io.Stderr, "coreslot: %s takes one ticket or hold id\n%s\n", verb, usage)
		return exitUsage
	}
	id, ok := wire.ParseName(pos[0])
	if !ok {
		fmt.Fprintf(io.Stderr, "coreslot: %q is not a ticket or hold id (the id column of `coreslot status`)\n", pos[0])
		return exitUsage
	}
	var already bool
	c := ticketCommand{verb: verb, noTicket: "no live ticket or hold %s (it ended, or was cancelled or removed)",
		nothing: "nothing was changed", publish: func(e *engine.Engine, ctx context.Context, id wire.ID) (err error) {
			already, err = e.MarkContext(ctx, dir, id)
			return err
		}}
	if *off {
		c.verb, c.noTicket = verb+" --off", "%s carries no such mark, and no live hold of its run does"
		c.publish = func(e *engine.Engine, ctx context.Context, id wire.ID) error {
			if removed, err := e.UnmarkContext(ctx, dir, id); err != nil || removed > 0 {
				return err
			}
			return engine.ErrNoTicket
		}
	}
	if _, _, code := c.request(id, false, "", io); code >= 0 {
		return code
	}
	switch {
	case *off:
		fmt.Fprintf(io.Stdout, "coreslot: "+unmarked+"\n", id)
	case already:
		fmt.Fprintf(io.Stdout, "coreslot: %s was already marked\n", id)
	default:
		fmt.Fprintf(io.Stdout, "coreslot: "+marked+"\n", id)
	}
	return 0
}
