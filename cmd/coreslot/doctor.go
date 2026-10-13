//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/store"
)

const (
	// exitProblems is doctor's status when it found a problem (PROTOCOL.md section 19).
	exitProblems = 1
	// resetWait is how long --reset waits to enter the admission section before it refuses.
	resetWait = time.Second
	// wantDescriptors is what a grant of the largest size needs open at once: up to 512
	// tokens, the hold, the ticket, the lock and the standard streams, with margin.
	wantDescriptors = 600
)

// doctorCommand is `coreslot doctor [--clear] [--reset]`: it checks the directory, its filesystem,
// permissions, format, the configuration, the descriptor limits and the degraded record, and
// reports every finding. --clear removes the degraded record. --reset empties the state, but only
// when nothing is live and FORMAT names no format this client does not implement (PROTOCOL.md
// section 4.4). It exits 1 when it found a problem, else 3 while degraded mode is recorded, else 0;
// a refused --clear or --reset is 75 (PROTOCOL.md section 19).
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

	var loc location
	loc.dir, loc.cfgPath, loc.err = config.Locate()
	if *doClear || *doReset {
		if code := doctorAct(*doReset, loc, io); code != 0 {
			return code
		}
	}
	return doctorReport(loc, io)
}

// location is what config.Locate returned. doctor locates once: each lookup of the home directory
// may start getent.
type location struct {
	dir, cfgPath string
	err          error
}

// doctorAct performs --reset (which removes the degraded record too) or --clear.
func doctorAct(doReset bool, loc location, io IO) int {
	what := "doctor --clear"
	if doReset {
		what = "doctor --reset"
	}
	e, err := openReadOnlyAt(loc.dir, loc.cfgPath, loc.err)
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
		removed, err := e.Reset(ctx)
		var live *engine.LiveError
		var incomplete *store.ResetIncompleteError
		switch {
		case errors.As(err, &live):
			fmt.Fprintf(io.Stderr, "coreslot: doctor --reset refused: %s\n", text(strings.Join(live.Reasons, "; ")))
			return exitRefusal
		case errors.As(err, &incomplete):
			// The removal is best effort: everything else was removed. Say how much, and what not.
			fmt.Fprintf(io.Stderr, "coreslot: doctor --reset: removed %d entries; %d could not be removed: %s\n",
				removed, incomplete.Left, text(incomplete.Err.Error()))
			return exitProblems
		case err != nil:
			return doctorFailed(what, err, io)
		}
		fmt.Fprintf(io.Stdout, "reset: removed %d entries; kept admit.lock, res/ and config.toml\n", removed)
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
		return "FORMAT is malformed (" + err.Error() + "); `coreslot doctor --reset` repairs it when nothing is live", nil
	case present && !store.Understood(n):
		return fmt.Sprintf("format %d is not implemented by this client; use a coreslot that implements it", n), nil
	}
	return "", nil
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

	fsErr := store.CheckFilesystem(nearestExisting(dir))
	if fsErr != nil {
		problem("filesystem: %s", text(fsErr.Error()))
	} else {
		line("filesystem: host-local")
	}

	var rep engine.Report
	e, err := openReadOnlyAt(dir, cfgPath, nil)
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
		problem("format: FORMAT is malformed (%s); `coreslot doctor --reset` repairs it when nothing is live", text(rep.FormatError))
	case !rep.FormatUnderstood: // before absence: a FORMAT naming format 0 is present, not absent
		line("format: %d, which this client does not implement; runs use token-only mode", rep.Format)
	case !rep.FormatPresent:
		line("format: FORMAT not written yet (the first client writes 1)")
	default:
		line("format: %d", rep.Format)
	}

	switch {
	case err != nil:
		line("configuration: %s (not checked)", text(cfgPath))
	case strings.HasPrefix(rep.ConfigError, "configuration "):
		// The file's own error already names it ("configuration <path>: ..."), so it takes no
		// second label; any other reason (the CPU count, say) keeps the label.
		problem("%s", text(rep.ConfigError))
	case rep.ConfigError != "":
		problem("configuration: %s", text(rep.ConfigError))
	default:
		state := "present"
		if _, serr := os.Lstat(cfgPath); errors.Is(serr, os.ErrNotExist) {
			state = "absent, the defaults apply"
		}
		line("configuration: %s (%s)", text(cfgPath), state)
		for _, name := range slices.Sorted(maps.Keys(rep.Resources)) {
			ri := rep.Resources[name]
			line("resource %s: size %d, locked %d, free %d", name, ri.Size, ri.Locked, ri.Free)
		}
	}

	var lim syscall.Rlimit
	if gerr := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); gerr != nil {
		problem("descriptor limits: %v", gerr)
	} else {
		// The soft limit doctor was started with, the one a run started here would add 256 to;
		// getrlimit reports the limit the Go runtime raised it to (rlimit_unix.go).
		soft, hard := startNofileProbe(contextFDs(environ())).originalNofile(), uint64(lim.Max)
		line("descriptor limit: soft %s, hard %s; a grant of 512 tokens needs about %d", limitText(soft), limitText(hard), wantDescriptors)
		if hard < wantDescriptors {
			problem("the hard descriptor limit %s is below %d: a large grant cannot be held", limitText(hard), wantDescriptors)
		} else if soft < wantDescriptors {
			line("note: the soft limit is below %d; coreslot run gives its command %d more, within the hard limit", wantDescriptors, extraNofile)
		}
	}

	if err == nil {
		if d := rep.Degraded; d != nil {
			if d.Malformed {
				line("degraded: recorded, the record is unreadable; `coreslot doctor --clear` removes it")
			} else {
				line("degraded: %s (%d fail-open %s since %s); `coreslot doctor --clear` removes the record",
					text(d.Reason), d.Count, plural(d.Count, "run"), time.Unix(0, d.FirstNS).UTC().Format(time.RFC3339))
			}
		} else {
			line("degraded: no")
		}
		if rep.Section.Held {
			line("admission section: %s", sectionHolder(rep.Section, store.NowNS()))
		}
		if n := len(rep.DeadHolds); n > 0 {
			line("note: %d dead %s awaiting reconciliation", n, plural(int64(n), "hold"))
		}
		var ignored []string
		for _, d := range []string{store.QueueDir, store.HoldsDir, store.CancelDir} {
			if n := rep.Ignored[d]; n > 0 {
				ignored = append(ignored, fmt.Sprintf("%s %d", d, n))
			}
		}
		if len(ignored) > 0 {
			line("note: ignored entries (malformed or stray, never trusted or removed): %s", strings.Join(ignored, ", "))
		}
	}

	switch {
	case problems > 0:
		line("%d %s found", problems, plural(int64(problems), "problem"))
		return exitProblems
	case err == nil && rep.Degraded != nil:
		return exitDegraded
	}
	line("no problems found")
	return 0
}

// limitText prints a descriptor limit, "unlimited" for the infinite one.
func limitText(n uint64) string {
	if n >= 1<<62 {
		return "unlimited"
	}
	return fmt.Sprint(n)
}

// nearestExisting is dir, or its nearest ancestor that exists: where a missing state directory
// would be created.
func nearestExisting(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}
