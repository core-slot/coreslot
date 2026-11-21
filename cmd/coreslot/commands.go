package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/core-slot/coreslot/internal/wire"
)

// text escapes text the program does not control so that a message stays on one line and
// cannot drive the terminal: labels, paths (CORESLOT_STATE_DIR is the environment's), values of
// the environment, command names, and the errors that embed any of them. Human output escapes
// their control characters and newlines; JSON output encodes them.
func text(s string) string { return wire.EscapeHuman(s) }

// The usage and the flags of status, cancel, trim, doctor, stats and report are the same on every
// platform: on Windows, where there is no budget (PROTOCOL.md section 16), they still decide
// whether an invocation asks for help, which prints on stdout and exits 0 for every command
// (section 19).

const statusUsage = `usage: coreslot status [--json] [--docker] [--observe DURATION]

--docker reads Docker Desktop's VM and its running containers from the Docker socket.
--observe watches the queue for DURATION to find STALLED tickets; a ticket is
STALLED after 10s without a refresh, so use 10s or more.`

const cancelUsage = `usage: coreslot cancel <ticket-id>
       coreslot cancel --label LABEL`

const moveUsage = `usage: coreslot move <ticket-id> --first | --last | --before <ticket-id> | --after <ticket-id>
       coreslot move --label LABEL --first | --last | --before <ticket-id> | --after <ticket-id>

Changes a waiting ticket's place in the queue without ending its run: --first and --last put it
ahead of or behind every ticket waiting now, --before and --after next to another one. Tickets
that arrive later queue behind it as usual.`

const trimUsage = `usage: coreslot trim <hold-id>
       coreslot trim --label LABEL

Asks a running job's coreslot run to lower the job's reservation to what its
command uses, and to follow it from then on, up to what it asked for. The id is
the one coreslot status shows on the hold's line.`

func trimFlags() (fs *flag.FlagSet, label *string) {
	fs = newFlagSet("coreslot trim")
	return fs, fs.String("label", "", "trim the one live hold with this label")
}

const doctorUsage = `usage: coreslot doctor [--clear] [--reset]`

const statsUsage = `usage: coreslot stats [--label LABEL] [--json]

Shows, per label, the window of its last 20 counted runs in the run log, the
need run learns from it, and the label's entry in needs.toml.`

func statsFlags() (fs *flag.FlagSet, label *string, asJSON *bool) {
	fs = newFlagSet("coreslot stats")
	return fs, fs.String("label", "", "only this label"), fs.Bool("json", false, "print the JSON document (schema 1)")
}

const reportUsage = `usage: coreslot report [--days N] [--json]

Summarizes the run log's last N days (default 7): runs, waits, memory stops and
the heaviest labels. It does not measure what the runs would have done without
coreslot.`

// maxReportDays bounds --days, so that the window's start stays a valid time.
const maxReportDays = 36500

func reportFlags() (fs *flag.FlagSet, days *int, asJSON *bool) {
	fs = newFlagSet("coreslot report")
	return fs, fs.Int("days", 7, "the window: the last N days"), fs.Bool("json", false, "print the JSON document (schema 1)")
}

func statusFlags() (fs *flag.FlagSet, asJSON, docker *bool, observe *time.Duration) {
	fs = newFlagSet("coreslot status")
	return fs, fs.Bool("json", false, "print the JSON document (schema 1)"),
		fs.Bool("docker", false, "read Docker Desktop's VM and its running containers"),
		fs.Duration("observe", 0, "watch the queue this long, to find STALLED tickets")
}

func cancelFlags() (fs *flag.FlagSet, label *string) {
	fs = newFlagSet("coreslot cancel")
	return fs, fs.String("label", "", "cancel the one waiting ticket with this label")
}

func moveFlags() (fs *flag.FlagSet, label *string, first, last *bool, before, after *string) {
	fs = newFlagSet("coreslot move")
	return fs, fs.String("label", "", "move the one waiting ticket with this label"),
		fs.Bool("first", false, "put it ahead of every waiting ticket"),
		fs.Bool("last", false, "put it behind every waiting ticket"),
		fs.String("before", "", "put it just ahead of this waiting ticket"),
		fs.String("after", "", "put it just behind this waiting ticket")
}

func doctorFlags() (fs *flag.FlagSet, doClear, doReset *bool) {
	fs = newFlagSet("coreslot doctor")
	return fs, fs.Bool("clear", false, "remove the degraded-mode record"),
		fs.Bool("reset", false, "empty the state directory, if nothing is live; a run log of format 4 or 3 is kept")
}

// newFlagSet returns a flag set that returns its errors to the caller and prints nothing itself.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	return fs
}

// noBudget is status, cancel, trim, doctor, stats or report where there is no budget to inspect,
// cancel or repair, and no run log to learn from (Windows). A request for help, recognized by the command's own flag set exactly as on unix,
// prints the usage on stdout and exits 0; anything else says why the command cannot work and
// exits 70.
func noBudget(name, usage string, fs *flag.FlagSet, args []string, io IO) int {
	if errors.Is(fs.Parse(args), flag.ErrHelp) {
		fmt.Fprintln(io.Stdout, usage)
		return 0
	}
	fmt.Fprintf(io.Stderr, "coreslot: %s: Windows has no coreslot budget\n", name)
	return exitInternal
}
