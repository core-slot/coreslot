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

// The usage and the flags of status, cancel and doctor are the same on every platform: on
// Windows, where there is no budget (PROTOCOL.md section 16), they still decide whether an
// invocation asks for help, which prints on stdout and exits 0 for every command (section 19).

const statusUsage = `usage: coreslot status [--json] [--observe DURATION]

--observe watches the queue for DURATION to find STALLED tickets; a ticket is
STALLED after 10s without a refresh, so use 10s or more.`

const cancelUsage = `usage: coreslot cancel <ticket-id>
       coreslot cancel --label LABEL`

const doctorUsage = `usage: coreslot doctor [--clear] [--reset]`

func statusFlags() (fs *flag.FlagSet, asJSON *bool, observe *time.Duration) {
	fs = newFlagSet("coreslot status")
	return fs, fs.Bool("json", false, "print the JSON document (schema 1)"),
		fs.Duration("observe", 0, "watch the queue this long, to find STALLED tickets")
}

func cancelFlags() (fs *flag.FlagSet, label *string) {
	fs = newFlagSet("coreslot cancel")
	return fs, fs.String("label", "", "cancel the one waiting ticket with this label")
}

func doctorFlags() (fs *flag.FlagSet, doClear, doReset *bool) {
	fs = newFlagSet("coreslot doctor")
	return fs, fs.Bool("clear", false, "remove the degraded-mode record"),
		fs.Bool("reset", false, "empty the state directory, if nothing is live")
}

// newFlagSet returns a flag set that returns its errors to the caller and prints nothing itself.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	return fs
}

// noBudget is status, cancel or doctor where there is no budget to inspect, cancel or repair
// (Windows). A request for help, recognised by the command's own flag set exactly as on unix,
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
