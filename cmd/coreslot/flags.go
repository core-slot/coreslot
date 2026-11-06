package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
)

// runOptions are the flags of `coreslot run`, whose synopsis PROTOCOL.md section 21 gives (the
// CLI is not part of the protocol).
type runOptions struct {
	need        int64  // MiB, once resolveNeed ran
	mem         int64  // MiB from --mem, 0 when not given
	pinned      bool   // need is --mem or the needs file's pin: learning leaves it alone
	noRestart   bool   // no_restart in the needs file, or a container command: never stopped to free memory
	restart     string // --restart: "yes", "no", or "" when not given
	typicalS    int64  // the label's typical run time from the run log, 0 unknown
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

const runUsage = `usage: coreslot run --restart=yes|no [--mem SIZE] [--label L] [--timeout D]
       [--on-timeout run|fail] [--no-wait] [--strict] [--independent] [--same-group]
       [--refusal-code N] -- COMMAND...`

// runHelp is what `coreslot run --help` prints: the synopsis, then each flag with its default.
const runHelp = runUsage + `

Runs COMMAND once the budget grants it the memory it needs.

  --restart=yes|no      required for a command that coreslot could stop. When memory runs
                        short, coreslot kills the newest commands and runs them again from
                        scratch. yes: the command is safe to kill at any moment and to run
                        again (tests, builds, linters, type checks). no: never stop it; it
                        changes the world outside its own output (databases, migrations,
                        deploys, pushes, sending messages, writing shared files). When
                        unsure, no. A label marked no_restart in needs.toml and a command
                        named docker, docker-compose or podman are never stopped, whatever
                        this says, and need no flag; such a command keeps its Linux
                        oom_score_adj. Not required where the command is never stopped
                        anyway: in a terminal, with --same-group, inside another run's grant,
                        or outside the budget; there no only leaves the run's Linux
                        oom_score_adj as it was, and yes changes nothing
  --mem SIZE            memory the command needs, such as 3G or 768M, more than 0. Without it
                        the run takes the label's entry in needs.toml, beside the
                        configuration, else what the label's last 20 runs needed, else 1M
                        for a command named docker, docker-compose or podman, else the
                        smaller of 4G and a quarter of the machine's memory
  --label L             a name for the request, shown by status and kept in the run log, so
                        never a secret; it names the request's entry in needs.toml (default
                        the command's shape and its repository, such as "go test@proj")
  --timeout D           stop waiting after D from the start, restarts included, for example 90s
                        or 10m (default 0, no limit)
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
func parseRun(args []string, streams IO) (runOptions, error) {
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
	fs.Func("restart", "yes or no: may the command be stopped and run again", func(v string) error {
		if v != "yes" && v != "no" {
			return errors.New("want yes or no")
		}
		o.restart = v
		return nil
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
	if err := spec.ValidateLabel(o.label); err != nil {
		return o, err
	}
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

// errRestartMissing is the usage error of a run that could be stopped and does not say whether
// its command may be run again.
var errRestartMissing = &spec.UsageError{Msg: `--restart is required: when memory runs short, coreslot may kill this command and run it again from scratch
  --restart=yes  the command is safe to kill at any moment and to run again from scratch
                 (tests, builds, linters, type checks)
  --restart=no   anything that changes the world outside its own output (databases,
                 migrations, deploys, pushes, sending messages, writing shared files)
When unsure, --restart=no.
` + runUsage}

// restartable reports whether a stop round may stop the command and run it again: --restart=yes
// and no no_restart mark on its label, which wins.
func (o runOptions) restartable() bool { return o.restart == "yes" && !o.noRestart }

// restartMissing reports whether the run must refuse with errRestartMissing: it could be a stop
// round's victim (its command gets a group of its own, unless shared says it shares coreslot's;
// its label is not no_restart) and --restart was not given.
func (o runOptions) restartMissing(shared bool) bool {
	return o.restart == "" && !o.noRestart && !shared
}

// containerCommand reports whether the command itself is a container client: argv[0] names one by
// its basename. A wrapped command (sh -c, env, time) is not.
func (o runOptions) containerCommand() bool {
	return len(o.argv) > 0 && memsample.ContainerProgram(filepath.Base(o.argv[0]))
}

func usagef(format string, args ...any) error {
	return &spec.UsageError{Msg: fmt.Sprintf(format, args...)}
}

// request is the request of the run. Its hold says that the run's process tree carries the work.
func (o runOptions) request() spec.Request {
	return spec.Request{Label: o.label, NeedMiB: o.need, TypicalS: o.typicalS, RealMemory: o.containerCommand() && !o.pinned}
}

// needText is the request in the form of the waiting line: `mem 3 GiB`.
func (o runOptions) needText() string { return "mem " + spec.FormatMiB(o.need) }
