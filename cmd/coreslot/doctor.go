//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
)

const (
	// exitProblems is doctor's status when it found a problem (PROTOCOL.md section 19).
	exitProblems = 1
	// resetWait is how long --reset waits to enter the admission section before it refuses.
	resetWait = time.Second
)

// doctorCommand is `coreslot doctor [--clear] [--reset]`: it checks the directory, its filesystem,
// permissions, format, the configuration and the degraded record, and
// reports every finding. --clear removes the degraded record. --reset recreates the state as this
// client's format, whatever format it had, keeping the run log of format 2, but only when nothing
// is live (PROTOCOL.md section 4.4). It exits 1 when it found a problem, else 3 while degraded
// mode is recorded, else 0; a refused --clear or --reset is 75 (PROTOCOL.md section 19).
func doctorCommand(args []string, io IO) int {
	fs, doClear, doReset := doctorFlags()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(io.Stdout, doctorUsage)
			return 0
		}
		fmt.Fprintf(io.Stderr, "coreslot: %s\n%s\n", text(err.Error()), doctorUsage)
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(io.Stderr, "coreslot: doctor: unexpected arguments\n%s\n", doctorUsage)
		return exitUsage
	}

	loc := locate()
	if *doClear || *doReset {
		if code := doctorAct(*doReset, loc, io); code != 0 {
			return code
		}
	}
	return doctorReport(loc, io)
}

// doctorAct performs --reset (which removes the degraded record too) or --clear.
func doctorAct(doReset bool, loc location, io IO) int {
	what := "doctor --clear"
	if doReset {
		what = "doctor --reset"
	}
	e, err := openEngine(loc, true)
	if err != nil {
		return doctorFailed(what, err, io)
	}
	if !e.Store().Exists() { // nothing has run: there is nothing to clear or reset, and nothing to create
		if doReset {
			fmt.Fprintln(io.Stdout, "reset: nothing to reset, no state directory yet (nothing has run)")
		} else {
			fmt.Fprintln(io.Stdout, "clear: no degraded record")
		}
		return 0
	}
	if doReset {
		ctx, cancel := context.WithTimeout(context.Background(), resetWait)
		defer cancel()
		done, err := e.Reset(ctx)
		var live *engine.LiveError
		var incomplete *store.ResetIncompleteError
		switch {
		case errors.As(err, &live):
			fmt.Fprintf(io.Stderr, "coreslot: doctor --reset refused: %s\n", text(strings.Join(live.Reasons, "; ")))
			return exitRefusal
		case errors.As(err, &incomplete):
			// The removal is best effort: everything else was removed. Say how much, and what not.
			fmt.Fprintf(io.Stderr, "coreslot: doctor --reset: removed %d entries%s; %d could not be removed: %s\n",
				done.Removed, resetLogNote(done), incomplete.Left, text(incomplete.Err.Error()))
			return exitProblems
		case err != nil:
			return doctorFailed(what, err, io)
		}
		kept := "admit.lock and config.toml"
		if done.LogKept > 0 {
			kept = "admit.lock, config.toml and the run log"
		}
		fmt.Fprintf(io.Stdout, "reset: removed %d entries%s; kept %s; wrote FORMAT\n", done.Removed, resetLogNote(done), kept)
		return 0
	}
	why, err := formatRefusal(e.Store())
	if err != nil {
		return doctorFailed(what, err, io)
	}
	if why != "" {
		fmt.Fprintf(io.Stderr, "coreslot: doctor --clear refused: %s\n", text(why))
		return exitRefusal
	}
	rec, err := e.Store().Degraded()
	if err != nil {
		return doctorFailed(what, err, io)
	}
	if rec == nil {
		fmt.Fprintln(io.Stdout, "clear: no degraded record")
		return 0
	}
	if err := e.Store().ClearDegraded(); err != nil {
		return doctorFailed(what, err, io)
	}
	fmt.Fprintln(io.Stdout, "clear: removed the degraded record")
	return 0
}

// resetLogNote says, after the count of what a reset removed, that the run log was among it and
// why; empty when the reset removed no entry of log/.
func resetLogNote(done store.ResetDone) string {
	switch {
	case done.LogRemoved == 0:
		return ""
	case done.FormatErr != nil:
		return ", the run log among them because FORMAT could not be read, so every label learns its memory need again"
	}
	return ", the run log of another format among them, so every label learns its memory need again"
}

// doctorFailed reports an error that kept --clear or --reset from acting. A state error
// (PROTOCOL.md section 14) is a problem doctor found, exit 1; anything else is an internal
// error, exit 70.
func doctorFailed(what string, err error, io IO) int {
	fmt.Fprintf(io.Stderr, "coreslot: %s: %s\n", what, text(err.Error()))
	if _, stateErr := classify(err); stateErr {
		return exitProblems
	}
	return exitInternal
}

// formatRefusal says why a write into the directory is refused for its FORMAT, or "": a
// malformed FORMAT, or a well-formed one this client does not implement (PROTOCOL.md section 14:
// a client never writes into a directory whose format it has not understood). A FORMAT that
// cannot be read at all is not a refusal but a state error, returned as err.
func formatRefusal(st *store.Store) (why string, err error) {
	n, present, err := st.ReadFormat()
	switch {
	case err != nil && !present:
		return "", err
	case err != nil:
		return "FORMAT is malformed (" + err.Error() + "); `coreslot doctor --reset` rewrites it when nothing is live, removing the run log", nil
	case present && !store.Understood(n):
		return (&store.FormatError{N: n}).Error(), nil
	}
	return "", nil
}

// doctorReadings reports whether each reading the watchdog stands on can be read, and the age of
// the newest machine sample. A reading the platform may lack (page wait, PSI) is not a problem.
func doctorReadings(w engine.WatchInfo, line, problem func(format string, args ...any)) {
	check := func(name string, err error) {
		if err != nil {
			problem("reading %s: %s", name, text(err.Error()))
		} else {
			line("reading %s: readable", name)
		}
	}
	host, err := memsample.ReadHost()
	check("of paging counters, swap and clocks", err)
	var headroomErr error
	if _, _, ok := memsample.Headroom(); !ok {
		headroomErr = errors.New("cannot be read")
	}
	check("of free memory", headroomErr)
	var timesErr error
	_, times, ok := memsample.Tree(os.Getpid())
	if !ok {
		timesErr = errors.New("cannot be read")
	}
	check("of process times", timesErr)
	if ok && !times[os.Getpid()].HasPageWait {
		line("reading of page wait: not reported by this system (optional)")
	}
	if runtime.GOOS == "linux" && !host.HasPSI {
		line("reading of memory pressure (PSI): not reported by this kernel (optional)")
	}
	if w.SampledNS == 0 {
		line("newest machine sample: none yet (no job has run)")
	} else {
		line("newest machine sample: %s old, level %s", duration(time.Duration(max(store.NowNS()-w.SampledNS, 0))), w.Level)
	}
}

// doctorReport prints the findings at loc and returns doctor's exit status.
func doctorReport(loc location, io IO) int {
	problems := 0
	line := func(format string, args ...any) { fmt.Fprintf(io.Stdout, format+"\n", args...) }
	problem := func(format string, args ...any) {
		problems++
		line("PROBLEM: "+format, args...)
	}

	dir, cfgPath := loc.dir, loc.cfgPath
	if loc.err != nil {
		problem("state directory: %s", text(loc.err.Error()))
		return exitProblems
	}
	source := "default location"
	if os.Getenv(config.StateDirEnv) != "" { // what makes Locate use it
		source = config.StateDirEnv
	}
	line("state directory: %s (%s)", text(dir), source)
	if _, serr := os.Lstat(dir); errors.Is(serr, os.ErrNotExist) {
		line("no state directory yet (nothing has run)")
	}

	fsErr := store.CheckFilesystem(store.NearestExisting(dir))
	if fsErr != nil {
		problem("filesystem: %s", text(fsErr.Error()))
	} else {
		line("filesystem: host-local")
	}

	var rep engine.Report
	e, err := openEngine(loc, true)
	if err == nil {
		rep, err = e.Snapshot()
	}
	switch {
	case err != nil && fsErr != nil:
		line("permissions: not checked (the filesystem is refused)")
	case err != nil:
		problem("permissions and layout: %s", text(err.Error()))
	case !rep.StateDirExists:
		line("permissions: nothing to check yet")
	default:
		line("permissions: every entry of the layout is the user's, a regular file or directory, not group- or world-writable")
	}

	switch {
	case err != nil:
	case rep.FormatError != "":
		problem("format: FORMAT is malformed (%s); `coreslot doctor --reset` rewrites it when nothing is live, removing the run log", text(rep.FormatError))
	case !rep.FormatUnderstood: // before absence: a FORMAT naming format 0 is present, not absent
		problem("format: %d, which this client does not implement; runs fail open until `coreslot doctor --reset`", rep.Format)
	case !rep.FormatPresent:
		line("format: FORMAT not written yet (the first client writes 2)")
	default:
		line("format: %d", rep.Format)
	}

	switch {
	case err != nil:
		line("configuration: %s (not checked)", text(cfgPath))
	case strings.HasPrefix(rep.ConfigError, "configuration "):
		// The file's own error already names it ("configuration <path>: ..."), so it takes no
		// second label; any other reason (the machine's memory, say) keeps the label.
		problem("%s", text(rep.ConfigError))
	case rep.ConfigError != "":
		problem("configuration: %s", text(rep.ConfigError))
	default:
		state := "present"
		if _, serr := os.Lstat(cfgPath); errors.Is(serr, os.ErrNotExist) {
			state = "absent, the defaults apply"
		}
		line("configuration: %s (%s)", text(cfgPath), state)
		if m := rep.Mem; m.Known {
			line("mem: reserved %s, free %s", spec.FormatMiB(m.ReservedMiB), spec.FormatMiB(m.FreeMiB))
		}
	}

	if err == nil {
		if d := rep.Degraded; d != nil {
			if d.Malformed {
				line("degraded: recorded, the record is unreadable; `coreslot doctor --clear` removes it")
			} else {
				line("degraded: %s (%d fail-open %s since %s); `coreslot doctor --clear` removes the record",
					text(d.Reason), d.Count, plural(d.Count, "run", "runs"), time.Unix(0, d.FirstNS).UTC().Format(time.RFC3339))
			}
		} else {
			line("degraded: no")
		}
		doctorReadings(rep.Watch, line, problem)
		if rep.Section.Held {
			line("admission section: %s", sectionHolder(rep.Section, store.NowNS()))
		}
		if n := len(rep.DeadHolds); n > 0 {
			line("note: %d dead %s awaiting reconciliation", n, plural(int64(n), "hold", "holds"))
		}
		for _, h := range rep.Holds {
			if h.OwnerExited { // a designed state, not a problem: the command keeps its inherited grant
				line("note: a hold whose owner exited is held by a descendant (%s, pid %d); its reservation frees when that process exits",
					labelText(h.Label), h.PID)
			}
		}
		if ignored := ignoredText(rep.Ignored); ignored != "" {
			line("note: ignored entries (malformed or stray, never trusted or removed): %s", ignored)
		}
	}

	switch {
	case problems > 0:
		line("%d %s found", problems, plural(int64(problems), "problem", "problems"))
		return exitProblems
	case err == nil && rep.Degraded != nil:
		return exitDegraded
	}
	line("no problems found")
	return 0
}
