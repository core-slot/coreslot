//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/store"
)

// watchCommand is `coreslot watch`: it sets or removes this machine's override of the watchdog's
// mode (PROTOCOL.md section 7.15), never the configuration file. It refuses (exit 75) as `move` does.
func watchCommand(args []string, io IO) int {
	fs, forD := watchFlags()
	positional, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(io.Stdout, watchUsage)
			return 0
		}
		fmt.Fprintf(io.Stderr, "coreslot: %s\n%s\n", text(err.Error()), watchUsage)
		return exitUsage
	}
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "for" })
	verb := firstOf(positional)
	if len(positional) != 1 || verb != "observe" && verb != "enforce" && verb != "config" ||
		set && (verb == "config" || *forD <= 0) {
		fmt.Fprintf(io.Stderr, "coreslot: watch takes observe, enforce (with a positive --for) or config\n%s\n", watchUsage)
		return exitUsage
	}
	mode, until, now := verb, int64(0), store.NowNS()
	if set && forD.Nanoseconds() > math.MaxInt64-now { // an end the clock cannot hold would read as none
		fmt.Fprintf(io.Stderr, "coreslot: watch: --for %s ends past the clock's range\n%s\n", *forD, watchUsage)
		return exitUsage
	}
	if verb == "config" {
		mode = ""
	} else if set {
		until = now + forD.Nanoseconds()
	}

	e, err := openEngine(locate(), true)
	if err != nil {
		return commandFailed("watch", err, io)
	}
	if !e.Store().Exists() {
		fmt.Fprintln(io.Stderr, "coreslot: watch: no state directory yet (nothing has run); the mode was not changed")
		return exitRefusal
	}
	ctx, stop := context.WithTimeout(context.Background(), cancelWait)
	defer stop()
	var busy *engine.SectionBusyError
	switch err := e.SetWatchModeContext(ctx, mode, until); {
	case errors.As(err, &busy), errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(io.Stderr, "coreslot: watch refused: %s; the mode was not changed\n", sectionBusyText(e, cancelWait))
		return exitRefusal
	case err != nil:
		return commandFailed("watch", err, io)
	case mode == "":
		fmt.Fprintln(io.Stdout, "coreslot: the watchdog follows the configuration's mode on this machine")
	default:
		fmt.Fprintf(io.Stdout, "coreslot: the watchdog is in %s mode on this machine, %s (`coreslot watch config` returns to the configuration)\n", mode, switchedText(until, now))
	}
	return 0
}

// modeName is the mode as the configuration spells it.
func modeName(observe bool) string {
	if observe {
		return "observe"
	}
	return "enforce"
}

// switchedText says how long a switch by hand holds: until switched back, or until its end, with
// its date when the end is not on nowNS's day.
func switchedText(untilNS, nowNS int64) string {
	if untilNS == 0 {
		return "switched by hand until switched back"
	}
	end, layout := time.Unix(0, untilNS), "15:04"
	if y, m, d := time.Unix(0, nowNS).Date(); end.Year() != y || end.Month() != m || end.Day() != d {
		layout = "2006-01-02 15:04"
	}
	return "switched by hand until " + end.Format(layout)
}
