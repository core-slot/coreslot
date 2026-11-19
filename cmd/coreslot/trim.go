//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/wire"
)

// trimAnswerWait is how long trim waits for the hold's run to answer its request, and
// trimAnswerPoll how often it looks; tests shorten the wait.
var trimAnswerWait = 8 * time.Second

const trimAnswerPoll = 250 * time.Millisecond

// trimCommand is `coreslot trim`: it asks the run of a live hold, named by the id `status` shows
// (or the hold's full name) or by its label, to lower the hold's reservation to what its command
// uses and to follow it. The request is written into trim.json inside the admission section;
// the run reads it at its tick and answers in its hold's body, which trim then waits for outside
// the section. A request that can do nothing (no such live hold, a hold that reserves nothing, a
// command that has exited), a label that names no hold or several, a state directory that cannot
// be used or whose format this client does not implement, and an admission section held for
// longer than cancelWait are refusals (exit 75). A run that does not answer is not one: exit 0.
func trimCommand(args []string, io IO) int {
	fs, label := trimFlags()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(io.Stdout, trimUsage)
			return 0
		}
		fmt.Fprintf(io.Stderr, "coreslot: %s\n%s\n", text(err.Error()), trimUsage)
		return exitUsage
	}
	byLabel := false
	fs.Visit(func(f *flag.Flag) { byLabel = byLabel || f.Name == "label" })
	if byLabel == (fs.NArg() > 0) || fs.NArg() > 1 {
		fmt.Fprintf(io.Stderr, "coreslot: trim takes a hold id or --label\n%s\n", trimUsage)
		return exitUsage
	}
	var id wire.ID
	if !byLabel {
		var ok bool
		if id, ok = wire.ParseName(fs.Arg(0)); !ok {
			fmt.Fprintf(io.Stderr, "coreslot: %q is not a hold id (the id coreslot status shows on a hold's line)\n", fs.Arg(0))
			return exitUsage
		}
	}

	e, err := openEngine(locate(), true)
	if err != nil {
		return commandFailed("trim", err, io)
	}
	if !e.Store().Exists() { // nothing has run, so nothing holds; do not create the directory for it
		fmt.Fprintln(io.Stderr, "coreslot: trim: no state directory yet (nothing has run), so no hold is live")
		return exitRefusal
	}
	rep, err := e.Snapshot()
	if err != nil {
		return commandFailed("trim", err, io)
	}
	if byLabel {
		var ids []string
		for _, h := range rep.Holds {
			if h.Label == *label && h.NeedMiB > 0 {
				ids = append(ids, h.ID)
			}
		}
		slices.Sort(ids)
		switch len(ids) {
		case 0:
			fmt.Fprintf(io.Stderr, "coreslot: trim: no live hold reserving memory has the label %s\n", text(*label))
			return exitRefusal
		case 1:
			id, _ = wire.ParseID(ids[0])
		default:
			fmt.Fprintf(io.Stderr, "coreslot: trim: %d live holds have the label %s; trim one by id: %s\n",
				len(ids), text(*label), strings.Join(ids, ", "))
			return exitRefusal
		}
	}
	before, seen := findHold(rep, id)
	name := id.String() // what the hold is called in a message: its label once a snapshot shows it
	if seen {
		name = labelText(before.Label)
	}

	ctx, stop := context.WithTimeout(context.Background(), cancelWait)
	defer stop()
	// TrimContext gives up entering the section when ctx ends, writing nothing. Only that is a
	// busy section: an error after a timely entry is reported as itself.
	var busy *engine.SectionBusyError
	switch err := e.TrimContext(ctx, id); {
	case err == nil:
	case errors.Is(err, engine.ErrNoHold):
		fmt.Fprintf(io.Stderr, "coreslot: trim: no live hold %s (it has ended, or the id is not a hold's)\n", id)
		return exitRefusal
	case errors.Is(err, engine.ErrNothingReserved):
		fmt.Fprintf(io.Stderr, "coreslot: trim: %s reserves nothing (it runs outside the budget), so there is nothing to lower\n", name)
		return exitRefusal
	case errors.Is(err, engine.ErrCommandExited):
		fmt.Fprintf(io.Stderr, "coreslot: trim: the command of %s has exited; its run is releasing the hold\n", name)
		return exitRefusal
	case errors.As(err, &busy), errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(io.Stderr, "coreslot: trim refused: %s; no request was written\n", sectionBusyText(e, cancelWait))
		return exitRefusal
	default:
		return commandFailed("trim", err, io)
	}

	if seen && before.Trimmed {
		fmt.Fprintf(io.Stdout, "coreslot: %s already follows what it uses (reserves %s, asked %s)\n",
			name, spec.FormatMiB(before.NeedMiB), spec.FormatMiB(askedMiB(before)))
		containerLine(io.Stdout, before)
		return 0
	}
	h, answered, gone := awaitTrim(e, id)
	switch {
	case gone:
		fmt.Fprintf(io.Stdout, "coreslot: %s: asked, but the hold ended before its run answered\n", name)
		return 0
	case !answered:
		fmt.Fprintf(io.Stdout, "coreslot: %s: asked; the run has not answered yet. A run started by an older coreslot never will; see coreslot status\n", name)
		return 0
	}
	name = labelText(h.Label)
	if h.NeedMiB < askedMiB(h) {
		fmt.Fprintf(io.Stdout, "coreslot: %s reserves %s now (asked %s) and follows what it uses\n",
			name, spec.FormatMiB(h.NeedMiB), spec.FormatMiB(askedMiB(h)))
	} else {
		fmt.Fprintf(io.Stdout, "coreslot: %s follows what it uses from now on; nothing is released yet (asked %s); coreslot status shows its reservation when it falls\n",
			name, spec.FormatMiB(askedMiB(h)))
	}
	containerLine(io.Stdout, h)
	return 0
}

// awaitTrim polls the hold id for up to trimAnswerWait until its run says it acted on the
// request (trimmed), and returns it. gone is a hold that a snapshot no longer shows; a snapshot
// that fails is read again.
func awaitTrim(e *engine.Engine, id wire.ID) (h engine.HolderInfo, answered, gone bool) {
	deadline := time.Now().Add(trimAnswerWait)
	for {
		if r, err := e.Snapshot(); err == nil {
			got, ok := findHold(r, id)
			if !ok || got.Trimmed {
				return got, ok, !ok
			}
		}
		if !time.Now().Before(deadline) {
			return h, false, false
		}
		time.Sleep(min(trimAnswerPoll, time.Until(deadline)))
	}
}

// findHold is the live hold id in rep.
func findHold(rep engine.Report, id wire.ID) (engine.HolderInfo, bool) {
	for _, h := range rep.Holds {
		if h.ID == id.String() {
			return h, true
		}
	}
	return engine.HolderInfo{}, false
}

// askedMiB is the need h was admitted with: its body's asked_mib, or need_mib when it has none.
func askedMiB(h engine.HolderInfo) int64 {
	if h.AskedMiB > 0 {
		return h.AskedMiB
	}
	return h.NeedMiB
}

// containerLine says, for container work, what a trimmed reservation cannot follow.
func containerLine(w io.Writer, h engine.HolderInfo) {
	if h.Container {
		fmt.Fprintln(w, "coreslot: its containers' memory is not measured, so its reservation follows the client's own memory")
	}
}
