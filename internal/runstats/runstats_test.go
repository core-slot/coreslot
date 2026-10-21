//go:build unix

package runstats

import (
	"slices"
	"testing"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
)

// t0 is 2026-10-20T00:00:00Z.
const t0 = int64(1792454400e9)

func f(v float64) *float64 { return &v }
func k(v int64) *int64     { return &v }

// gib is size GiB in KiB, rounded up.
func gib(size float64) *int64 { return k(int64(size*1024*1024 + 0.999)) }

// run is a counted run of label at t0+i seconds that used cores over 10 s, with a cpu need of
// need and a grant of grant.
func run(label string, i int64, cores float64, need spec.Range, grant int) store.RunRecord {
	return store.RunRecord{TimeNS: t0 + i*1e9, Label: label, RanS: 10, CPUUserS: f(cores * 10), CPUSysS: f(0),
		Need: map[string]spec.Range{"cpu": need}, Grant: map[string]int{"cpu": grant}}
}

func only(t *testing.T, stats []Stats) Stats {
	t.Helper()
	if len(stats) != 1 {
		t.Fatalf("%d Stats, want 1: %+v", len(stats), stats)
	}
	return stats[0]
}

// The window is the last 5 counted runs of a label by time_ns, whatever order the log has them in.
// Mutation: a window of all runs instead of the last 5.
func TestWindowIsLastFive(t *testing.T) {
	var runs []store.RunRecord
	for _, i := range []int64{6, 0, 1, 2, 3, 4, 5} {
		runs = append(runs, run("x", i, float64(i+1), spec.Range{Min: 1, Max: 1}, 1))
	}
	s := only(t, Summarize(runs, 16))
	if s.Runs != 7 || len(s.Window) != 5 {
		t.Fatalf("Runs %d, window %d; want 7 and 5", s.Runs, len(s.Window))
	}
	for n, r := range s.Window {
		if want := t0 + int64(n+2)*1e9; r.TimeNS != want {
			t.Errorf("window[%d] at %d, want %d", n, r.TimeNS, want)
		}
	}
	if want := []float64{3, 4, 5, 6, 7}; !slices.Equal(s.CoresUsed, want) {
		t.Errorf("CoresUsed %v, want %v", s.CoresUsed, want)
	}
}

// mem is the window's largest peak, a failed run's included, rounded up to a whole token.
// Mutation: leave failed runs out of the peak, or round to the nearest token.
func TestMemIsMaxRoundedUp(t *testing.T) {
	r1, r2, r3 := run("x", 1, 1, spec.Range{Min: 1, Max: 1}, 1), run("x", 2, 1, spec.Range{Min: 1, Max: 1}, 1), run("x", 3, 1, spec.Range{Min: 1, Max: 1}, 1)
	r1.MemPeakKiB, r2.MemPeakKiB, r3.MemPeakKiB = gib(2.1), gib(2.7), gib(3.0)
	r3.Exit = 1
	s := only(t, Summarize([]store.RunRecord{r1, r2, r3}, 16))
	if s.Suggest.MemMiB != 3072 || s.MemPeakKiB != *gib(3.0) {
		t.Errorf("mem %d MiB, peak %d KiB; want 3072 and %d", s.Suggest.MemMiB, s.MemPeakKiB, *gib(3.0))
	}
	s = only(t, Summarize([]store.RunRecord{r2}, 16))
	if s.Suggest.MemMiB != 2816 {
		t.Errorf("mem %d MiB, want 2816", s.Suggest.MemMiB)
	}
	s = only(t, Summarize([]store.RunRecord{run("x", 1, 1, spec.Range{Min: 1, Max: 1}, 1)}, 16))
	if s.Suggest.MemMiB != 0 || s.MemPeakKiB != 0 {
		t.Errorf("no peak recorded, yet mem %d MiB, peak %d KiB", s.Suggest.MemMiB, s.MemPeakKiB)
	}
}

// cpu is the window's most cores used, rounded up and clamped to the cpu size; a range need stays
// a range from 1.
// Mutation: round the cores to the nearest integer, or drop the clamp to cpuSize.
func TestCPUIsMaxCeilClamped(t *testing.T) {
	ranged := []store.RunRecord{run("x", 1, 3.2, spec.Range{Min: 4, Max: 4}, 4), run("x", 2, 5.6, spec.Range{Min: 1, Max: 16}, 10)}
	if c := only(t, Summarize(ranged, 16)).Suggest.CPU; c == nil || *c != (spec.Range{Min: 1, Max: 6}) {
		t.Errorf("cpu %v, want 1..6", c)
	}
	if c := only(t, Summarize(ranged, 4)).Suggest.CPU; c == nil || *c != (spec.Range{Min: 1, Max: 4}) {
		t.Errorf("cpu on a size of 4: %v, want 1..4", c)
	}
	fixed := []store.RunRecord{ranged[1], run("x", 3, 3.2, spec.Range{Min: 4, Max: 4}, 4)}
	if c := only(t, Summarize(fixed, 16)).Suggest.CPU; c == nil || *c != (spec.Range{Min: 6, Max: 6}) {
		t.Errorf("cpu after a fixed need: %v, want 6..6", c)
	}
	unmeasured := run("x", 1, 0, spec.Range{Min: 1, Max: 1}, 1)
	unmeasured.CPUUserS = nil
	if c := only(t, Summarize([]store.RunRecord{unmeasured}, 16)).Suggest.CPU; c != nil {
		t.Errorf("cpu %v without a measured run, want none", c)
	}
}

// A run that used 0.8 of its cpu grant or more is saturated, and the suggestion is its grant.
// Mutation: compare with the need's max instead of the grant.
func TestSaturationKeepsTheGrant(t *testing.T) {
	s := only(t, Summarize([]store.RunRecord{run("x", 1, 3.9, spec.Range{Min: 1, Max: 16}, 4), run("x", 2, 3.1, spec.Range{Min: 1, Max: 16}, 4)}, 16))
	if s.Saturated != 1 {
		t.Errorf("Saturated %d, want 1", s.Saturated)
	}
	if c := s.Suggest.CPU; c == nil || c.Max != 4 {
		t.Errorf("cpu %v, want a max of 4", c)
	}
}

// Unlabelled, pass-through and fail-open runs say nothing about what a label needs.
// Mutation: count fail-open runs.
func TestUncountedSkipped(t *testing.T) {
	unlabelled := run("", 1, 1, spec.Range{Min: 1, Max: 1}, 1)
	nested := run("x", 2, 1, spec.Range{Min: 1, Max: 1}, 1)
	nested.Nested = true
	light := run("y", 3, 1, spec.Range{Min: 1, Max: 1}, 1)
	light.FailOpen = "light"
	timeout := run("z", 4, 1, spec.Range{Min: 1, Max: 1}, 1)
	timeout.FailOpen = "timeout"
	workersOnly := run("@5", 5, 1, spec.Range{Min: 1, Max: 1}, 1)
	if stats := Summarize([]store.RunRecord{unlabelled, nested, light, timeout, workersOnly}, 16); len(stats) != 0 {
		t.Errorf("Stats %+v, want none", stats)
	}
}

// A worker count at the end of a label does not make another label.
// Mutation: group by the full label.
func TestWorkerSuffixGroups(t *testing.T) {
	stats := Summarize([]store.RunRecord{run("x@10", 1, 1, spec.Range{Min: 1, Max: 1}, 1), run("x@4", 2, 1, spec.Range{Min: 1, Max: 1}, 1)}, 16)
	if s := only(t, stats); s.Label != "x" || s.Runs != 2 {
		t.Errorf("Stats %q with %d runs, want \"x\" with 2", s.Label, s.Runs)
	}
}

// Waiting is summed over the window; a run without waited_free_s adds 0.
// Mutation: take the last run's waited_free_s instead of the sum.
func TestWaitedFreeSummed(t *testing.T) {
	a, b, c := run("x", 1, 1, spec.Range{Min: 1, Max: 1}, 1), run("x", 2, 1, spec.Range{Min: 1, Max: 1}, 1), run("x", 3, 1, spec.Range{Min: 1, Max: 1}, 1)
	a.WaitedFreeS, b.WaitedFreeS = f(3), f(4)
	a.WaitedS, b.WaitedS, c.WaitedS = 5, 6, 1
	s := only(t, Summarize([]store.RunRecord{a, b, c}, 16))
	if s.WaitedFreeS != 7 || s.WaitedS != 12 {
		t.Errorf("waited %v, waited while free %v; want 12 and 7", s.WaitedS, s.WaitedFreeS)
	}
}
