//go:build unix

package main

import (
	"fmt"
	"slices"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/runstats"
	"github.com/core-slot/coreslot/internal/store"
)

// Bounds of the automatic preemptible rule: a label is demoted when it has at least autoMinRuns
// counted runs whose median ran_s is at least autoMedianS seconds.
const (
	autoMinRuns = 5
	autoMedianS = 600
)

// deadlineHint is what the first demotion of a label says.
const deadlineHint = "Runs with wall-clock deadlines (test timeouts, latency assertions, browser or e2e tests, " +
	"dev-server compile windows, agent tool timeouts) can fail when demoted; set `preemptible = false` for them."

// autoPreemptible is the automatic rule for a run that queues: o is preemptible when neither the
// flag nor the entry said anything, it has a label, it is not in terminal mode, its cpu request's
// max is at least half cpuSize (a small claim gains nothing from shrinking) and its label has the
// history. The size is tested first, so read, which returns the run log, is called only for a
// large request. reason is "" when the run is not demoted, else the text the record and status
// show; first reports that no earlier record of the label has a preemptible reason. A run log
// that cannot be read demotes nothing.
func autoPreemptible(o runOptions, terminal bool, cpuSize int, read func() ([]store.RunRecord, error)) (reason string, first bool) {
	cpu, ok := o.need["cpu"]
	half := (cpuSize + 1) / 2
	if o.preemptible != nil || o.label == "" || terminal || !ok || cpuSize < 1 || cpu.Max < half {
		return "", false
	}
	all, err := read()
	if err != nil {
		return "", false
	}
	runs := runstats.LabelRuns(all, o.label)
	if len(runs) < autoMinRuns || runstats.Unobserved(runs) {
		return "", false
	}
	ran := make([]float64, len(runs))
	for i, r := range runs {
		ran[i] = r.RanS
	}
	slices.Sort(ran)
	median := (ran[(len(ran)-1)/2] + ran[len(ran)/2]) / 2
	if median < autoMedianS {
		return "", false
	}
	first = !slices.ContainsFunc(runs, func(r store.RunRecord) bool { return r.Preemptible != "" })
	return fmt.Sprintf("auto: median %dm >= %dm, cpu %d >= %d", int(median)/60, autoMedianS/60, cpu.Max, half), first
}

// autoPreempt applies the automatic rule to the run before it queues, and says so the first time
// the label is demoted.
func (r *run) autoPreempt(e *engine.Engine) {
	cfg, err := e.Config()
	if err != nil {
		return
	}
	m, err := config.ReadMachine(false) // only the cpu size is read below
	if err != nil {
		return
	}
	read := func() ([]store.RunRecord, error) {
		runs, _, err := e.Store().ReadRuns()
		return runs, err
	}
	reason, first := autoPreemptible(r.o, terminalMode(r.io.Stdin), cfg.Sizes(m)["cpu"], read)
	if reason == "" {
		return
	}
	r.o.preemptWhy = reason
	if first {
		r.notice("%s", deadlineHint)
	}
}
