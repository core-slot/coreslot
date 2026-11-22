//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/store"
)

// watchCommand is `coreslot watch`: it sets or removes this machine's override of the watchdog's
// mode (PROTOCOL.md section 7.15), never the configuration file. It refuses (exit 75) as `move` does.
func watchCommand(args []string, io IO) int {
	fs, forD := watchFlags()
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprintln(io.Stdout, watchUsage)
				return 0
			}
			fmt.Fprintf(io.Stderr, "coreslot: %s\n%s\n", text(err.Error()), watchUsage)
			return exitUsage
		}
		if fs.NArg() == 0 {
			break
		}
		positional, args = append(positional, fs.Arg(0)), fs.Args()[1:]
	}
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "for" })
	verb := firstOf(positional)
	if len(positional) != 1 || verb != "observe" && verb != "enforce" && verb != "config" ||
		set && (verb == "config" || *forD < time.Minute) {
		fmt.Fprintf(io.Stderr, "coreslot: watch takes observe, enforce (with --for of 1m or more) or config\n%s\n", watchUsage)
		return exitUsage
	}
	mode, until := verb, int64(0)
	if verb == "config" {
		mode = ""
	} else if set {
		until = store.NowNS() + forD.Nanoseconds()
	}

	e, err := openEngine(locate(), true)
	if err != nil {
		return commandFailed("watch", err, io)
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
		fmt.Fprintf(io.Stdout, "coreslot: the watchdog is in %s mode on this machine, %s\n", mode, modeSourceText("override", until))
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

// modeSourceText says where the mode comes from: the configuration, or the override and its end.
func modeSourceText(source string, untilNS int64) string {
	switch {
	case source != "override":
		return "from the configuration"
	case untilNS == 0:
		return "switched by hand until switched back (`coreslot watch config` returns to the configuration)"
	}
	return "switched by hand until " + time.Unix(0, untilNS).Format("15:04") + " (`coreslot watch config` returns to the configuration)"
}
