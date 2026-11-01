package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
)

// runOptions are the flags of `coreslot run`, whose synopsis PROTOCOL.md section 21 gives (the
// CLI is not part of the protocol).
type runOptions struct {
	need        int64 // MiB: --mem, then the needs file, then spec.DefaultNeedMiB
	mem         int64 // MiB from --mem, 0 when not given
	label       string
	timeout     time.Duration
	onTimeout   string // "fail" (the default) or "run"
	noWait      bool
	strict      bool
	independent bool
	sameGroup   bool // --same-group: the command stays in coreslot's process group
	refusalCode int
	argv        []string
}

const runUsage = `usage: coreslot run [--mem SIZE] [--label L] [--timeout D] [--on-timeout run|fail]
       [--no-wait] [--strict] [--independent] [--same-group] [--refusal-code N] -- COMMAND...`

// runHelp is what `coreslot run --help` prints: the synopsis, then each flag with its default.
const runHelp = runUsage + `

Runs COMMAND once the budget grants it the memory it needs.

  --mem SIZE            memory the command needs, such as 3G or 768M, more than 0. A labelled
                        run takes it from the label's entry in needs.toml, beside the
                        configuration; with neither, 128M
  --label L             a name for the request, shown by status and kept in the run log, so
                        never a secret; it names the request's entry in needs.toml (default
                        none)
  --timeout D           stop waiting after D, for example 90s or 10m (default 0, no limit)
  --on-timeout run|fail when --timeout passes: fail refuses, run runs the command outside the
                        budget (default fail)
  --no-wait             refuse unless the request is admitted at once
  --strict              refuse instead of running unbudgeted when the state directory or the
                        configuration cannot be used
  --independent         queue as a request of its own, even inside another run's grant
  --same-group          keep COMMAND in coreslot's own process group instead of a group of its
                        own, so that a caller that kills coreslot's group (SIGKILL included)
                        reaches the whole command tree. As in a terminal, a SIGINT or SIGQUIT
                        sent to coreslot's pid alone is then dropped and a SIGTERM or SIGHUP
                        reaches only COMMAND's pid: signal the group, not coreslot's pid. No
                        effect on Windows
  --refusal-code N      exit status of a refusal, 1 to 255 (default 75)`

// errHelp is returned for -h and --help: the help was printed on stdout and the run exits 0.
var errHelp = errors.New("help requested")

// parseRun parses the arguments of `coreslot run`. Every error is a *spec.UsageError (exit 64),
// detected before anything queues: a bad flag value, a size that is not one, or a missing
// command.
//
// lookup, when not nil, finds the needs file's entry for a label, whose mem the request takes
// when --mem was not given.
func parseRun(args []string, streams IO, lookup needsLookup) (runOptions, error) {
	o := runOptions{onTimeout: "fail", refusalCode: exitRefusal}
	fs := flag.NewFlagSet("coreslot run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Func("mem", "memory the command needs, such as 3G or 768M", func(v string) error {
		mib, err := spec.ParseBytes(v)
		if err == nil && mib == 0 {
			err = errors.New("want more than 0")
		}
		o.mem = mib
		return err
	})
	fs.StringVar(&o.label, "label", "", "label of the request")
	fs.DurationVar(&o.timeout, "timeout", 0, "give up waiting after this long")
	fs.StringVar(&o.onTimeout, "on-timeout", "fail", "run or fail when --timeout passes")
	fs.BoolVar(&o.noWait, "no-wait", false, "refuse unless admitted at once")
	fs.BoolVar(&o.strict, "strict", false, "refuse instead of failing open")
	fs.BoolVar(&o.independent, "independent", false, "ignore an inherited grant and queue")
	fs.BoolVar(&o.sameGroup, "same-group", false, "keep the command in coreslot's process group")
	fs.IntVar(&o.refusalCode, "refusal-code", exitRefusal, "exit status of a refusal")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(streams.Stdout, runHelp)
			return o, errHelp
		}
		return o, usagef("%s\n%s", text(err.Error()), runUsage) // names an unknown flag as given
	}
	o.argv = fs.Args()
	if len(o.argv) == 0 {
		return o, usagef("no command given\n%s", runUsage)
	}
	mib := o.mem
	if mib == 0 && o.label != "" && lookup != nil {
		if e, ok := lookup(o.label); ok {
			mib = e.MemMiB
		}
	}
	req := spec.Normalize(spec.Request{Label: o.label, NeedMiB: mib})
	if err := req.Validate(); err != nil {
		return o, err
	}
	o.need = req.NeedMiB
	switch {
	case o.onTimeout != "fail" && o.onTimeout != "run":
		return o, usagef("--on-timeout %q: want run or fail", o.onTimeout)
	case o.timeout < 0:
		return o, usagef("--timeout %v: want a duration of 0 or more (0 means no limit)", o.timeout)
	case o.refusalCode < 1 || o.refusalCode > 255:
		return o, usagef("--refusal-code %d: want 1 to 255", o.refusalCode)
	}
	return o, nil
}

func usagef(format string, args ...any) error {
	return &spec.UsageError{Msg: fmt.Sprintf(format, args...)}
}

// request is the request of the run. Its hold says that the run's process tree carries the work.
func (o runOptions) request() spec.Request {
	return spec.Request{Label: o.label, NeedMiB: o.need, Tree: true}
}

// needText is the request in the form of the waiting line: `mem 3 GiB`.
func (o runOptions) needText() string { return "mem " + spec.FormatMiB(o.need) }
