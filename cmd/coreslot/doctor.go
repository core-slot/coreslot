//go:build unix

package main

import (
	"bytes"
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
	"github.com/Nezhinskiy/coreslot/internal/testhooks"
)

const doctorUsage = `usage: coreslot doctor [--clear] [--reset]`

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
// a refused reset is 75.
func doctorCommand(args []string, io IO) int {
	fs := flag.NewFlagSet("coreslot doctor", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	doClear := fs.Bool("clear", false, "remove the degraded-mode record")
	doReset := fs.Bool("reset", false, "empty the state directory, if nothing is live")
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

	if *doClear || *doReset {
		if code := doctorAct(*doReset, io); code != 0 {
			return code
		}
	}
	return doctorReport(io)
}

// doctorAct performs --reset (which removes the degraded record too) or --clear.
func doctorAct(doReset bool, io IO) int {
	e, _, err := openEngineReadOnly()
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: doctor: %s\n", text(err.Error()))
		return exitInternal
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
		var removed int
		ctx, cancel := context.WithTimeout(context.Background(), resetWait)
		defer cancel()
		err := inSection(func() (err error) {
			removed, err = e.Reset(ctx)
			return err
		})
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
			return exitInternal
		case err != nil:
			fmt.Fprintf(io.Stderr, "coreslot: doctor --reset: %s\n", text(err.Error()))
			return exitInternal
		}
		fmt.Fprintf(io.Stdout, "reset: removed %d entries; kept admit.lock, res/ and config.toml\n", removed)
		return 0
	}
	if why := formatRefusal(e.Store()); why != "" {
		fmt.Fprintf(io.Stderr, "coreslot: doctor --clear refused: %s\n", text(why))
		return exitRefusal
	}
	rec, err := e.Store().Degraded()
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: doctor --clear: %s\n", text(err.Error()))
		return exitInternal
	}
	if rec == nil {
		fmt.Fprintln(io.Stdout, "clear: no degraded record")
		return 0
	}
	if err := inSection(e.Store().ClearDegraded); err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: doctor --clear: %s\n", text(err.Error()))
		return exitInternal
	}
	fmt.Fprintln(io.Stdout, "clear: removed the degraded record")
	return 0
}

// formatRefusal says why a write into the directory is refused for its FORMAT, or "": a
// malformed FORMAT, or a well-formed one this client does not implement (PROTOCOL.md section 14:
// a client never writes into a directory whose format it has not understood).
func formatRefusal(st *store.Store) string {
	n, present, err := st.ReadFormat()
	switch {
	case err != nil:
		return "FORMAT is malformed (" + err.Error() + "); `coreslot doctor --reset` repairs it when nothing is live"
	case present && !slices.Contains(testhooks.Formats(), n):
		return fmt.Sprintf("format %d is not implemented by this client; use a coreslot that implements it", n)
	}
	return ""
}

// doctorReport prints the findings and returns doctor's exit status.
func doctorReport(io IO) int {
	problems := 0
	line := func(format string, args ...any) { fmt.Fprintf(io.Stdout, format+"\n", args...) }
	problem := func(format string, args ...any) {
		problems++
		line("PROBLEM: "+format, args...)
	}

	dir, override, err := config.StateDir()
	if err != nil {
		problem("state directory: %s", text(err.Error()))
		return exitProblems
	}
	source := "default location"
	if override {
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
	e, _, err := openEngineReadOnly()
	if err == nil {
		err = inSection(func() (err error) { // Snapshot probes admit.lock
			rep, err = e.Snapshot()
			return err
		})
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
	case rep.Format == 0:
		line("format: FORMAT not written yet (the first client writes 1)")
	case !rep.FormatUnderstood:
		line("format: %d, which this client does not implement; runs use token-only mode", rep.Format)
	default:
		line("format: %d", rep.Format)
	}

	cfgPath, cerr := config.Path()
	switch {
	case cerr != nil:
		problem("configuration: %s", text(cerr.Error()))
	case err != nil:
		line("configuration: %s (not checked)", text(cfgPath))
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
		soft, hard := uint64(lim.Cur), uint64(lim.Max)
		line("descriptor limit: soft %s, hard %s; a grant of 512 tokens needs about %d", limitText(soft), limitText(hard), wantDescriptors)
		if hard < wantDescriptors {
			problem("the hard descriptor limit %s is below %d: a large grant cannot be held", limitText(hard), wantDescriptors)
		} else if soft < wantDescriptors {
			line("note: the soft limit is below %d; coreslot run raises its own, within the hard limit", wantDescriptors)
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
			line("admission section: held right now")
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
