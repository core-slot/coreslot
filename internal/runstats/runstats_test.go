//go:build unix

package runstats

import (
	"testing"

	"github.com/core-slot/coreslot/internal/store"
)

// t0 is 2026-10-20T00:00:00Z.
const t0 = int64(1792454400e9)

func f(v float64) *float64 { return &v }
func k(v int64) *int64     { return &v }

// gib is size GiB in KiB, rounded up.
func gib(size float64) *int64 { return k(int64(size*1024*1024 + 0.999)) }

// run is a counted run of label at t0+i seconds.
func run(label string, i int64) store.RunRecord {
	return store.RunRecord{TimeNS: t0 + i*1e9, Label: label, RanS: 10}
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
		runs = append(runs, run("x", i))
	}
	s := only(t, Summarize(runs))
	if s.Runs != 7 || len(s.Window) != 5 {
		t.Fatalf("Runs %d, window %d; want 7 and 5", s.Runs, len(s.Window))
	}
	for n, r := range s.Window {
		if want := t0 + int64(n+2)*1e9; r.TimeNS != want {
			t.Errorf("window[%d] at %d, want %d", n, r.TimeNS, want)
		}
	}
}

// mem is the window's largest peak, a failed run's included, rounded up to a whole MiB.
// Mutation: leave failed runs out of the peak, or round down (2764).
func TestMemIsMaxRoundedUp(t *testing.T) {
	r1, r2, r3 := run("x", 1), run("x", 2), run("x", 3)
	r1.MemPeakKiB, r2.MemPeakKiB, r3.MemPeakKiB = gib(2.1), gib(2.7), gib(3.0)
	r3.Exit = 1
	s := only(t, Summarize([]store.RunRecord{r1, r2, r3}))
	if s.Suggest.MemMiB != 3072 || s.MemPeakKiB != *gib(3.0) {
		t.Errorf("mem %d MiB, peak %d KiB; want 3072 and %d", s.Suggest.MemMiB, s.MemPeakKiB, *gib(3.0))
	}
	s = only(t, Summarize([]store.RunRecord{r2}))
	if s.Suggest.MemMiB != 2765 {
		t.Errorf("mem %d MiB, want 2765", s.Suggest.MemMiB)
	}
	s = only(t, Summarize([]store.RunRecord{run("x", 1)}))
	if s.Suggest.MemMiB != 0 || s.MemPeakKiB != 0 {
		t.Errorf("no peak recorded, yet mem %d MiB, peak %d KiB", s.Suggest.MemMiB, s.MemPeakKiB)
	}
}

// The evidence names the window, the peak with its measure and when it was read.
// Mutation: drop the source, or the time, from the peak.
func TestEvidenceNamesThePeak(t *testing.T) {
	r1, r2 := run("x", 1), run("x", 2)
	r1.MemPeakKiB, r1.MemPeakSource, r1.PeakAfterS = gib(1.0), "footprint", f(3)
	r2.MemPeakKiB, r2.MemPeakSource, r2.PeakAfterS = gib(2.7), "footprint", f(22.4)
	s := only(t, Summarize([]store.RunRecord{r1, r2}))
	if want := "2 runs 2026-10-20: peak 2.7 GiB (footprint) at 22s"; s.Suggest.Reason != want {
		t.Errorf("reason %q, want %q", s.Suggest.Reason, want)
	}
	if s := only(t, Summarize([]store.RunRecord{run("x", 1)})); s.Suggest.Reason != "1 run 2026-10-20" {
		t.Errorf("reason %q without a peak, want the window alone", s.Suggest.Reason)
	}
}

// Unlabelled, pass-through and fail-open runs say nothing about what a label needs.
// Mutation: count fail-open runs.
func TestUncountedSkipped(t *testing.T) {
	unlabelled := run("", 1)
	nested := run("x", 2)
	nested.Nested = true
	timeout := run("z", 4)
	timeout.FailOpen = "timeout"
	if stats := Summarize([]store.RunRecord{unlabelled, nested, timeout}); len(stats) != 0 {
		t.Errorf("Stats %+v, want none", stats)
	}
}

// Waiting is summed over the window; a run without waited_free_s adds 0.
// Mutation: take the last run's waited_free_s instead of the sum.
func TestWaitedFreeSummed(t *testing.T) {
	a, b, c := run("x", 1), run("x", 2), run("x", 3)
	a.WaitedFreeS, b.WaitedFreeS = f(3), f(4)
	a.WaitedS, b.WaitedS, c.WaitedS = 5, 6, 1
	s := only(t, Summarize([]store.RunRecord{a, b, c}))
	if s.WaitedFreeS != 7 || s.WaitedS != 12 {
		t.Errorf("waited %v, waited while free %v; want 12 and 7", s.WaitedS, s.WaitedFreeS)
	}
}
