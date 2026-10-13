package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// runOptions are the flags of `coreslot run`, whose synopsis PROTOCOL.md section 21 gives (the
// CLI is not part of the protocol).
type runOptions struct {
	need        map[string]spec.Range // normalized; defaultNeed when no --need is given
	label       string
	priority    int
	prioritySet bool // --priority given: it overrides the configuration's [priorities]
	timeout     time.Duration
	onTimeout   string // "fail" (the default) or "run"
	noWait      bool
	strict      bool
	independent bool
	subst       bool
	refusalCode int
	argv        []string
}

const runUsage = `usage: coreslot run [--need RES=MIN..MAX]... [--label L] [--priority P] [--timeout D]
       [--on-timeout run|fail] [--no-wait] [--strict] [--independent] [--subst]
       [--refusal-code N] -- COMMAND...`

// runHelp is what `coreslot run --help` prints: the synopsis, then each flag with its default.
const runHelp = runUsage + `

Runs COMMAND once the budget grants it between MIN and MAX tokens of each resource it needs.

  --need RES=MIN..MAX   tokens of RES the command needs; repeatable. RES=N means N..N,
                        RES=MIN.. means MIN..max(MIN, 16) and RES=..MAX means 1..MAX
                        (default cpu=1..16 when COMMAND has a cpu placeholder, else cpu=1..1)
  --label L             a name for the request, shown by status and kept in the run log, so
                        never a secret (default none)
  --priority P          -1000 to 1000: the request queues as if it arrived P minutes earlier
                        (default the configuration's [priorities] entry for the label, else 0)
  --timeout D           stop waiting after D, for example 90s or 10m (default 0, no limit)
  --on-timeout run|fail when --timeout passes: fail refuses, run runs the command outside the
                        budget with MIN of each resource (default fail)
  --no-wait             refuse unless the request is admitted at once
  --strict              refuse instead of running unbudgeted when the state directory or the
                        configuration cannot be used
  --independent         queue as a request of its own, even inside another run's grant
  --subst               replace placeholders inside arguments too, as in make -j{n}
  --refusal-code N      exit status of a refusal, 1 to 255 (default 75)

An argument that is exactly {n} becomes the number of cpu tokens granted, {n:RES} the number of
RES tokens; {{n}} and {{n:RES}} stand for the literal text. A cpu placeholder is {n} or {n:cpu}
as an argument, or inside one with --subst.`

type needList []string

func (n *needList) String() string     { return strings.Join(*n, ",") }
func (n *needList) Set(v string) error { *n = append(*n, v); return nil }

// errHelp is returned for -h and --help: the help was printed on stdout and the run exits 0.
var errHelp = errors.New("help requested")

// parseRun parses the arguments of `coreslot run`. Every error is a *spec.UsageError (exit 64),
// detected before anything queues: a malformed or duplicate need, an invalid request, a bad flag
// value, a missing command, or a placeholder naming a resource the request does not name.
func parseRun(args []string, streams IO) (runOptions, error) {
	o := runOptions{onTimeout: "fail", refusalCode: exitRefusal}
	fs := flag.NewFlagSet("coreslot run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var needs needList
	fs.Var(&needs, "need", "RES=MIN..MAX, RES=MIN.., RES=..MAX or RES=N (repeatable)")
	fs.StringVar(&o.label, "label", "", "label of the request")
	fs.IntVar(&o.priority, "priority", 0, "priority in [-1000, 1000]")
	fs.DurationVar(&o.timeout, "timeout", 0, "give up waiting after this long")
	fs.StringVar(&o.onTimeout, "on-timeout", "fail", "run or fail when --timeout passes")
	fs.BoolVar(&o.noWait, "no-wait", false, "refuse unless admitted at once")
	fs.BoolVar(&o.strict, "strict", false, "refuse instead of failing open")
	fs.BoolVar(&o.independent, "independent", false, "ignore an inherited grant and queue")
	fs.BoolVar(&o.subst, "subst", false, "replace placeholders inside arguments too")
	fs.IntVar(&o.refusalCode, "refusal-code", exitRefusal, "exit status of a refusal")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(streams.Stdout, runHelp)
			return o, errHelp
		}
		return o, usagef("%s\n%s", text(err.Error()), runUsage) // names an unknown flag as given
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "priority" {
			o.prioritySet = true
		}
	})
	o.argv = fs.Args()
	if len(o.argv) == 0 {
		return o, usagef("no command given\n%s", runUsage)
	}
	need, err := spec.ParseNeeds(needs)
	if err != nil {
		return o, err
	}
	if len(need) == 0 {
		need = defaultNeed(o.argv, o.subst)
	}
	req := spec.Normalize(spec.Request{Label: o.label, Priority: o.priority, Need: need})
	if err := req.Validate(); err != nil {
		return o, err
	}
	o.need = req.Need
	switch {
	case o.onTimeout != "fail" && o.onTimeout != "run":
		return o, usagef("--on-timeout %q: want run or fail", o.onTimeout)
	case o.timeout < 0:
		return o, usagef("--timeout %v: want a duration of 0 or more (0 means no limit)", o.timeout)
	case o.refusalCode < 1 || o.refusalCode > 255:
		return o, usagef("--refusal-code %d: want 1 to 255", o.refusalCode)
	}
	for _, res := range placeholderResources(o.argv, o.subst) {
		if _, ok := o.need[res]; !ok {
			return o, usagef("a placeholder names %s, which the request does not name", res)
		}
	}
	return o, nil
}

// defaultNeed is the request of a run without --need: cpu=1..spec.DefaultMax when the command
// takes a cpu placeholder (`{n}` or `{n:cpu}`, inside an argument too with --subst), and cpu=1..1
// otherwise. A command that cannot learn n would leave every token beyond its first idle while
// others wait for them; one that declares it uses n can put a range to work.
func defaultNeed(argv []string, inside bool) map[string]spec.Range {
	hi := 1
	if slices.Contains(placeholderResources(argv, inside), "cpu") {
		hi = spec.DefaultMax
	}
	return map[string]spec.Range{"cpu": {Min: 1, Max: hi}}
}

func usagef(format string, args ...any) error {
	return &spec.UsageError{Msg: fmt.Sprintf(format, args...)}
}

// request is the request of the run, with priority as given: the caller applies the
// configuration's [priorities] unless --priority was given.
func (o runOptions) request(priority int) spec.Request {
	return spec.Request{Label: o.label, Priority: priority, Need: o.need}
}

// sortedNeed lists the requested resources in ascending name order.
func (o runOptions) sortedNeed() []string {
	names := make([]string, 0, len(o.need))
	for name := range o.need {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// needText is the request in the form of the waiting line: `cpu 4..12, pg-local 1..1`.
func (o runOptions) needText() string { return needText(o.need) }

// needText is a request as the waiting line and status show it: `cpu 4..12, gpu 1..1`.
func needText(need map[string]spec.Range) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(need)) {
		parts = append(parts, fmt.Sprintf("%s %d..%d", name, need[name].Min, need[name].Max))
	}
	return strings.Join(parts, ", ")
}

// minGrant is each requested resource's min: the grant of a run without a budget.
func (o runOptions) minGrant() map[string]int {
	g := make(map[string]int, len(o.need))
	for name, r := range o.need {
		g[name] = r.Min
	}
	return g
}
