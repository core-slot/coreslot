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

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/needs"
	"github.com/core-slot/coreslot/internal/spec"
)

// runOptions are the flags of `coreslot run`, whose synopsis PROTOCOL.md section 21 gives (the
// CLI is not part of the protocol).
type runOptions struct {
	need        map[string]spec.Range // normalized: --need, then the needs file, then defaultNeed
	named       map[string]bool       // the resources --need named, or cpu when defaultNeed supplied it
	label       string
	priority    int
	prioritySet bool // --priority given: it overrides the configuration's [priorities]
	timeout     time.Duration
	onTimeout   string // "fail" (the default) or "run"
	noWait      bool
	strict      bool
	independent bool
	light       bool
	preemptible *bool  // --preemptible, else the needs entry's key; nil when neither says
	preemptWhy  string // why the run is preemptible: "flag", "entry" or the automatic rule's reason; "" if not
	subst       bool
	refusalCode int
	argv        []string

	// cpuPerWorker and workers are the label's cpu_per_worker and the worker width its entry
	// gave the cpu request; both are zero when cpu came from --need, from an entry without
	// cpu_per_worker, or from no entry.
	cpuPerWorker float64
	workers      spec.Range
}

const runUsage = `usage: coreslot run [--need RES=MIN..MAX]... [--label L] [--priority P] [--timeout D]
       [--on-timeout run|fail] [--no-wait] [--strict] [--independent] [--light] [--preemptible]
       [--subst] [--refusal-code N] -- COMMAND...`

// runHelp is what `coreslot run --help` prints: the synopsis, then each flag with its default.
const runHelp = runUsage + `

Runs COMMAND once the budget grants it between MIN and MAX tokens of each resource it needs.

  --need RES=MIN..MAX   tokens of RES the command needs; repeatable. RES=N means N..N,
                        RES=MIN.. means MIN..max(MIN, 16) and RES=..MAX means 1..MAX; memory
                        is one size, mem=3G or mem=768M. A labelled run takes the cpu and mem
                        it leaves out from the label's entry in needs.toml, beside the
                        configuration; with neither --need nor cpu there, the default applies
                        (default cpu=1..16, at most the CPU count, when COMMAND has a cpu
                        placeholder, else cpu=1..1)
  --label L             a name for the request, shown by status and kept in the run log, so
                        never a secret; it names the request's entry in needs.toml, without a
                        final @N or @- (default none)
  --priority P          -1000 to 1000: the request queues as if it arrived P minutes earlier
                        (default the configuration's [priorities] entry for the label, else 0)
  --timeout D           stop waiting after D, for example 90s or 10m (default 0, no limit)
  --on-timeout run|fail when --timeout passes: fail refuses, run runs the command outside the
                        budget with MIN of each resource (default fail)
  --no-wait             refuse unless the request is admitted at once
  --strict              refuse instead of running unbudgeted when the state directory or the
                        configuration cannot be used
  --independent         queue as a request of its own, even inside another run's grant
  --light               run at once outside the budget with MIN of each resource, for a
                        command that keeps well under one core busy (a download, a stream)
  --preemptible         admit with one cpu token and run the command at a lower priority
                        (nice 10) for the whole run (default the label's needs.toml entry,
                        else a long run of a large request is made so by itself)
  --subst               replace placeholders inside arguments too, as in make -j{n}
  --refusal-code N      exit status of a refusal, 1 to 255 (default 75)

An argument that is exactly {n} becomes the number of cpu tokens granted, {n:RES} the number of
RES tokens and {n:mem} the memory granted in MiB; {{n}} and {{n:RES}} stand for the literal
text. A cpu placeholder is {n} or {n:cpu}
as an argument, or inside one with --subst.`

type needList []string

func (n *needList) String() string     { return strings.Join(*n, ",") }
func (n *needList) Set(v string) error { *n = append(*n, v); return nil }

// errHelp is returned for -h and --help: the help was printed on stdout and the run exits 0.
var errHelp = errors.New("help requested")

// parseRun parses the arguments of `coreslot run`. Every error is a *spec.UsageError (exit 64),
// detected before anything queues: a malformed or duplicate need, an invalid request, a bad flag
// value, a missing command, or a placeholder naming a resource the request does not name.
//
// lookup, when not nil, finds the needs file's entry for a label. Each resource's range comes
// from --need, else from the entry for the run's label, else, for cpu and only when no --need
// was given at all, from defaultNeed. An entry that makes a valid request invalid is set aside
// with lookup's notice, and the run goes on without it: the needs file never refuses a run.
func parseRun(args []string, streams IO, lookup needsLookup) (runOptions, error) {
	o := runOptions{onTimeout: "fail", refusalCode: exitRefusal}
	fs := flag.NewFlagSet("coreslot run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var needArgs needList
	fs.Var(&needArgs, "need", "RES=MIN..MAX, RES=MIN.., RES=..MAX or RES=N (repeatable)")
	fs.StringVar(&o.label, "label", "", "label of the request")
	fs.IntVar(&o.priority, "priority", 0, "priority in [-1000, 1000]")
	fs.DurationVar(&o.timeout, "timeout", 0, "give up waiting after this long")
	fs.StringVar(&o.onTimeout, "on-timeout", "fail", "run or fail when --timeout passes")
	fs.BoolVar(&o.noWait, "no-wait", false, "refuse unless admitted at once")
	fs.BoolVar(&o.strict, "strict", false, "refuse instead of failing open")
	fs.BoolVar(&o.independent, "independent", false, "ignore an inherited grant and queue")
	fs.BoolVar(&o.light, "light", false, "run at once outside the budget")
	preemptible := fs.Bool("preemptible", false, "run the command at a lower priority")
	fs.BoolVar(&o.subst, "subst", false, "replace placeholders inside arguments too")
	fs.IntVar(&o.refusalCode, "refusal-code", exitRefusal, "exit status of a refusal")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(streams.Stdout, runHelp)
			return o, errHelp
		}
		return o, usagef("%s\n%s", text(err.Error()), runUsage) // names an unknown flag as given
	}
	var waitFlags []string // flags that shape a wait, which a --light run does not have
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "priority" {
			o.prioritySet = true
		}
		if f.Name == "preemptible" {
			o.preemptible = preemptible
			if *preemptible {
				o.preemptWhy = "flag"
			}
		}
		if slices.Contains([]string{"priority", "timeout", "on-timeout", "no-wait"}, f.Name) {
			waitFlags = append(waitFlags, "--"+f.Name)
		}
	})
	if o.light && len(waitFlags) > 0 {
		return o, usagef("--light does not wait, so %s has no meaning", strings.Join(waitFlags, " and "))
	}
	o.argv = fs.Args()
	if len(o.argv) == 0 {
		return o, usagef("no command given\n%s", runUsage)
	}
	need, err := spec.ParseNeeds(needArgs)
	if err != nil {
		return o, err
	}
	o.named = make(map[string]bool, len(need)+1)
	for name := range need {
		o.named[name] = true
	}
	withEntry := maps.Clone(need)
	var setAside func(error)
	var perWorker float64
	var workers spec.Range
	if o.label != "" && lookup != nil {
		if e, ok, ignore := lookup(o.label); ok {
			setAside = ignore
			if _, given := need["cpu"]; !given {
				switch {
				case e.CPUPerWorker > 0:
					cpus, err := machineCPUs()
					if err != nil || cpus <= 0 {
						cpus = spec.DefaultMax // as defaultNeed does when the count is unknown
					}
					withEntry["cpu"], workers = e.Request(needs.WorkersFromLabel(o.label), cpus)
					perWorker = e.CPUPerWorker
				case e.CPU != nil:
					withEntry["cpu"] = *e.CPU
				}
			}
			if _, given := need["mem"]; !given && e.MemMiB > 0 {
				t := spec.MemTokensCeil(e.MemMiB)
				withEntry["mem"] = spec.Range{Min: t, Max: t}
			}
			if o.preemptible == nil {
				o.preemptible = e.Preemptible
				if e.Preemptible != nil && *e.Preemptible {
					o.preemptWhy = "entry"
				}
			}
		}
	}
	req, err := o.validRequest(withEntry, len(needArgs) > 0)
	if err != nil && setAside != nil {
		if bare, berr := o.validRequest(need, len(needArgs) > 0); berr == nil {
			setAside(err)
			req, err = bare, nil
			perWorker, workers = 0, spec.Range{}
		}
	}
	if err != nil {
		return o, err
	}
	o.need = req.Need
	o.cpuPerWorker, o.workers = perWorker, workers
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

// marked reports whether the run is preemptible: --preemptible, the entry's key or the automatic
// rule said so.
func (o runOptions) marked() bool { return o.preemptWhy != "" }

// admitNeed is the request the budget sees. A preemptible run asks for one cpu token whatever its
// cpu request: it runs at nice 10 and gets idle cores anyway, so a larger minimum buys nothing.
// Its mem is never touched. o.need stays the unshrunk request, which the placeholders and the run
// log use.
func (o runOptions) admitNeed() map[string]spec.Range {
	if _, ok := o.need["cpu"]; !ok || !o.marked() {
		return o.need
	}
	need := maps.Clone(o.need)
	need["cpu"] = spec.Range{Min: 1, Max: 1}
	return need
}

// placeholderGrant is grant as the placeholders see it: a preemptible run's cpu is its unshrunk
// request's max, so that a `make -j{n}` keeps the width it was written for while the run holds
// one token.
func (o runOptions) placeholderGrant(grant map[string]int) map[string]int {
	cpu, ok := o.need["cpu"]
	if !ok || !o.marked() {
		return grant
	}
	g := maps.Clone(grant)
	g["cpu"] = cpu.Max
	return g
}

// validRequest is the validated request for need, which takes defaultNeed's cpu when it has none and
// no --need was given.
func (o *runOptions) validRequest(need map[string]spec.Range, given bool) (spec.Request, error) {
	if _, ok := need["cpu"]; !ok && !given {
		need["cpu"] = defaultNeed(o.argv, o.subst)["cpu"]
		o.named["cpu"] = true
	}
	req := spec.Normalize(spec.Request{Label: o.label, Priority: o.priority, Need: need})
	return req, req.Validate()
}

// defaultNeed is the request of a run without --need: cpu=1..min(spec.DefaultMax, the machine's
// CPU count) when the command takes a cpu placeholder (`{n}` or `{n:cpu}`, inside an argument too
// with --subst), and cpu=1..1 otherwise. A command that cannot learn n would leave every token
// beyond its first idle while others wait for them; one that declares it uses n can put a range to
// work, but never more workers than the machine has CPUs, which a cpu budget above 100 % allows.
func defaultNeed(argv []string, inside bool) map[string]spec.Range {
	hi := 1
	if slices.Contains(placeholderResources(argv, inside), "cpu") {
		hi = spec.DefaultMax
		if n, err := machineCPUs(); err == nil && n > 0 {
			hi = min(hi, n)
		}
	}
	return map[string]spec.Range{"cpu": {Min: 1, Max: hi}}
}

// machineCPUs is the machine's CPU count; tests replace it.
var machineCPUs = config.MachineCPUs

func usagef(format string, args ...any) error {
	return &spec.UsageError{Msg: fmt.Sprintf(format, args...)}
}

// request is the request of the run, with priority as given: the caller applies the
// configuration's [priorities] unless --priority was given. Its hold says that the run's process
// tree carries the work.
func (o runOptions) request(priority int) spec.Request {
	return spec.Request{Label: o.label, Priority: priority, Need: o.admitNeed(), Tree: true, Preemptible: o.preemptWhy}
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
func (o runOptions) needText() string { return needText(o.admitNeed()) }

// needText is a request as the waiting line and status show it: `cpu 4..12, gpu 1..1`, with a
// fixed mem need as one size, `mem 3 GiB`.
func needText(need map[string]spec.Range) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(need)) {
		r := need[name]
		if name == "mem" && r.Min == r.Max {
			parts = append(parts, "mem "+amount(name, r.Min))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s..%s", name, amount(name, r.Min), amount(name, r.Max)))
	}
	return strings.Join(parts, ", ")
}

// minGrant is each requested resource's min: the grant of a run without a budget.
func (o runOptions) minGrant() map[string]int {
	g := make(map[string]int, len(o.need))
	for name, r := range o.admitNeed() {
		g[name] = r.Min
	}
	return g
}
