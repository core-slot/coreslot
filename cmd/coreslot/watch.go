//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/store"
)

// watchCommand is `coreslot watch`: alone it prints the watchdog's mode and where it comes from;
// `observe` or `enforce` sets this machine's override (PROTOCOL.md section 7.15), with --for an end;
// `config` removes it, so the configuration's mode applies again. The configuration file is never
// written. A state directory that cannot be used or whose format this client does not implement,
// and an admission section held for longer than cancelWait, are refusals (exit 75).
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
	switch {
	case len(positional) > 1, verb != "" && verb != "observe" && verb != "enforce" && verb != "config",
		set && (verb == "" || verb == "config"), set && *forD < time.Minute:
		fmt.Fprintf(io.Stderr, "coreslot: watch takes observe, enforce (with --for of 1m or more) or config\n%s\n", watchUsage)
		return exitUsage
	}

	e, err := openEngine(locate(), verb != "")
	if err != nil {
		return commandFailed("watch", err, io)
	}
	if verb == "" {
		return printWatchMode(e, io.Stdout, io.Stderr)
	}
	mode, until := verb, int64(0)
	if verb == "config" {
		mode = ""
	} else if set {
		until = store.NowNS() + forD.Nanoseconds()
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
	}
	return printWatchMode(e, io.Stdout, io.Stderr)
}

// printWatchMode prints the watchdog's mode on this machine and where it comes from.
func printWatchMode(e *engine.Engine, stdout, stderr io.Writer) int {
	cfg, err := e.Config()
	if err != nil {
		fmt.Fprintf(stderr, "coreslot: watch: %s\n", text(err.Error()))
		return exitRefusal
	}
	_, source, until, _ := e.WatchMode(false)
	fmt.Fprintf(stdout, "coreslot: the watchdog is in %s mode on this machine (%s)\n", modeName(cfg.Watch.Observe),
		modeSourceText(source, until))
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
		return "switched by hand until switched back; `coreslot watch config` returns to the configuration"
	}
	return "switched by hand until " + time.Unix(0, untilNS).Format("15:04") + "; `coreslot watch config` returns to the configuration"
}
