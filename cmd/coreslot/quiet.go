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
	e, err := openEngine(locate(), true)
	if err != nil {
		return commandFailed(verb, err, io)
	}
	if !e.Store().Exists() { // nothing has run; do not create the directory for it
		fmt.Fprintf(io.Stderr, "coreslot: %s: no state directory yet (nothing has run), so no job is waiting or running\n", verb)
		return exitRefusal
	}
	ctx, stop := context.WithTimeout(context.Background(), cancelWait)
	defer stop()
	var already bool
	var removed int
	if *off {
		removed, err = e.UnmarkContext(ctx, dir, id)
	} else {
		already, err = e.MarkContext(ctx, dir, id)
	}
	var busy *engine.SectionBusyError
	switch {
	case err == nil && *off && removed == 0:
		fmt.Fprintf(io.Stderr, "coreslot: %s --off: %s carries no such mark, and no live hold of its run does\n", verb, id)
	case err == nil && *off:
		fmt.Fprintf(io.Stdout, "coreslot: "+unmarked+"\n", id)
		return 0
	case err == nil:
		if already {
			fmt.Fprintf(io.Stdout, "coreslot: %s was already marked\n", id)
		} else {
			fmt.Fprintf(io.Stdout, "coreslot: "+marked+"\n", id)
		}
		return 0
	case errors.Is(err, engine.ErrNoTicket):
		fmt.Fprintf(io.Stderr, "coreslot: %s: no live ticket or hold %s (it ended, or was cancelled or removed)\n", verb, id)
	case errors.As(err, &busy), errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(io.Stderr, "coreslot: %s refused: %s; nothing was changed\n", verb, sectionBusyText(e, cancelWait))
	default:
		return commandFailed(verb, err, io)
	}
	return exitRefusal
}
