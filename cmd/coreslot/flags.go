package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// runOptions are the flags of `coreslot run`.
type runOptions struct {
	need        map[string]spec.Range // as requested; cpu=1..1 when no --need is given
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

type needList []string

func (n *needList) String() string     { return strings.Join(*n, ",") }
func (n *needList) Set(v string) error { *n = append(*n, v); return nil }

// errHelp is returned for -h and --help: the usage was printed and the run exits 0.
var errHelp = errors.New("help requested")

// parseRun parses the arguments of `coreslot run`. Every error is a *spec.UsageError (exit 64),
// detected before anything queues: a malformed or duplicate need, an invalid request, a bad flag
// value, a missing command, or a placeholder naming a resource the request does not name.
func parseRun(args []string, stderr io.Writer) (runOptions, error) {
	o := runOptions{onTimeout: "fail", refusalCode: exitRefusal}
	fs := flag.NewFlagSet("coreslot run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var needs needList
	fs.Var(&needs, "need", "RES=MIN..MAX or RES=N (repeatable)")
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
			fmt.Fprintln(stderr, runUsage)
			return o, errHelp
		}
		return o, usagef("%v\n%s", err, runUsage)
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
	req := spec.Normalize(spec.Request{Label: o.label, Priority: o.priority, Need: need})
	if err := req.Validate(); err != nil {
		return o, err
	}
	o.need = req.Need
	switch {
	case o.onTimeout != "fail" && o.onTimeout != "run":
		return o, usagef("--on-timeout %q: want run or fail", o.onTimeout)
	case o.timeout < 0:
		return o, usagef("--timeout %v: want a positive duration", o.timeout)
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
func (o runOptions) needText() string {
	var parts []string
	for _, name := range o.sortedNeed() {
		r := o.need[name]
		parts = append(parts, fmt.Sprintf("%s %d..%d", name, r.Min, r.Max))
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
