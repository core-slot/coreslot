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

// The window is the last 20 counted runs of a label by time_ns, whatever order the log has them in.
// Mutation: a window of all runs instead of the last 20.
func TestWindowIsLastTwenty(t *testing.T) {
	runs := []store.RunRecord{run("x", 24)}
	for i := range int64(24) {
		runs = append(runs, run("x", i))
	}
	s := only(t, Summarize(runs))
	if s.Runs != 25 || len(s.Window) != 20 {
		t.Fatalf("Runs %d, window %d; want 25 and 20", s.Runs, len(s.Window))
	}
	for n, r := range s.Window {
		if want := t0 + int64(n+5)*1e9; r.TimeNS != want {
			t.Errorf("window[%d] at %d, want %d", n, r.TimeNS, want)
		}
	}
}

// With few runs the need is the largest peak, a failed run's included, rounded up to a whole MiB.
// Mutation: leave failed runs out of the peak, or round down (2764).
func TestMemIsMaxRoundedUp(t *testing.T) {
	r1, r2, r3 := run("x", 1), run("x", 2), run("x", 3)
	r1.MemPeakKiB, r2.MemPeakKiB, r3.MemPeakKiB = gib(2.1), gib(2.7), gib(3.0)
	r3.Exit = 1
	s := only(t, Summarize([]store.RunRecord{r1, r2, r3}))
	if s.NeedMiB != 3072 || s.MemPeakKiB != *gib(3.0) {
		t.Errorf("need %d MiB, peak %d KiB; want 3072 and %d", s.NeedMiB, s.MemPeakKiB, *gib(3.0))
	}
	s = only(t, Summarize([]store.RunRecord{r2}))
	if s.NeedMiB != 2765 {
		t.Errorf("need %d MiB, want 2765", s.NeedMiB)
	}
	s = only(t, Summarize([]store.RunRecord{run("x", 1)}))
	if s.NeedMiB != 0 || s.MemPeakKiB != 0 {
		t.Errorf("no peak recorded, yet need %d MiB, peak %d KiB", s.NeedMiB, s.MemPeakKiB)
	}
}

// The span names the window's runs and dates. Mutation: count every run of the label.
func TestSpanNamesTheWindow(t *testing.T) {
	if got := only(t, Summarize([]store.RunRecord{run("x", 1), run("x", 2)})).Span(); got != "2 runs 2026-10-20" {
		t.Errorf("span %q, want 2 runs 2026-10-20", got)
	}
}

// Unlabelled and fail-open runs say nothing about what a label needs.
// Mutation: count fail-open runs.
func TestUncountedSkipped(t *testing.T) {
	unlabelled := run("", 1)
	timeout := run("z", 4)
	timeout.FailOpen = "timeout"
	if stats := Summarize([]store.RunRecord{unlabelled, timeout}); len(stats) != 0 {
		t.Errorf("Stats %+v, want none", stats)
	}
}

// Waiting is summed over the window. Mutation: take the last run's waited_s instead of the sum.
func TestWaitedSummed(t *testing.T) {
	a, b, c := run("x", 1), run("x", 2), run("x", 3)
	a.WaitedS, b.WaitedS, c.WaitedS = 5, 6, 1
	if s := only(t, Summarize([]store.RunRecord{a, b, c})); s.WaitedS != 12 {
		t.Errorf("waited %v, want 12", s.WaitedS)
	}
}

// The need is the p90 of the window's peaks: of 20 peaks, 1 to 20 GiB, the 19th.
// Mutation: the maximum (20 GiB), or the 18th by nearest rank (18 GiB).
func TestNeedIsP90(t *testing.T) {
	var runs []store.RunRecord
	for i := int64(1); i <= 20; i++ {
		r := run("x", i)
		r.MemPeakKiB = gib(float64(i))
		runs = append(runs, r)
	}
	if got, ok := NeedMiB(Window(runs, "x", WindowRuns)); !ok || got != 19*1024 {
		t.Errorf("NeedMiB = %d, %v; want %d, true", got, ok, 19*1024)
	}
}

// A run that was stopped says nothing about what its label needs.
// Mutation: count a run with stops.
func TestStoppedRunsNotCounted(t *testing.T) {
	ok, stopped := run("x", 1), run("x", 2)
	stopped.Stops = 1
	if w := Window([]store.RunRecord{ok, stopped}, "x", WindowRuns); len(w) != 1 || w[0].TimeNS != ok.TimeNS {
		t.Errorf("Window = %+v, want only the run that was not stopped", w)
	}
}

// The normal share is the median of the window's tick shares; a run without one adds nothing, and
// a run's user_s and sys_s are not a tick share.
// Mutation: read user_s and sys_s as well, and the median is 0.4.
func TestNormalShareIsMedianOfTickShares(t *testing.T) {
	var window []store.RunRecord
	for i, share := range []float64{0.1, 0.5, 0.3} {
		r := run("x", int64(i))
		r.TickShare = f(share)
		window = append(window, r)
	}
	timed := run("x", 8)
	timed.UserS, timed.SysS = f(1), f(9)
	window = append(window, timed, run("x", 9))
	if got, ok := NormalShare(window); !ok || got != 0.3 {
		t.Errorf("NormalShare = %v, %v; want 0.3, true", got, ok)
	}
}

// A window whose runs have only user_s and sys_s is learning.
// Mutation: derive the share from sys_s/(user_s+sys_s) when there is no tick share.
func TestTimeWithoutTickSharesIsNotLearned(t *testing.T) {
	r := run("x", 1)
	r.UserS, r.SysS = f(1), f(1)
	if got, ok := NormalShare([]store.RunRecord{r}); ok {
		t.Errorf("NormalShare = %v, true; want it unknown", got)
	}
}

// The typical run time is the p90 of the window's ran_s rounded up, at least 1 s; an empty window
// has none. Mutations: the largest instead of the p90 gives 20, rounding down 18, no floor 0.
func TestTypicalIsP90RoundedUp(t *testing.T) {
	var window []store.RunRecord
	for i := range int64(20) {
		r := run("x", i)
		r.RanS = float64(i) + 0.2
		window = append(window, r)
	}
	instant := run("x", 0)
	instant.RanS = 0
	if p90, one, none := TypicalS(window), TypicalS([]store.RunRecord{instant}), TypicalS(nil); p90 != 19 || one != 1 || none != 0 {
		t.Errorf("TypicalS = %d, %d, %d; want 19, 1 and 0", p90, one, none)
	}
}

// A label's typical CPU time is the median of user_s + sys_s over the window's runs that report
// both, and unknown below MinCPURuns of them. Mutations: counting a run with one key reaches 10
// with 9; taking the mean gives 12.
func TestTypicalCPUS(t *testing.T) {
	var window []store.RunRecord
	for i := range int64(9) {
		r := run("x", i)
		r.UserS, r.SysS = f(float64(i+1)), f(1)
		window = append(window, r)
	}
	halfKnown := run("x", 9)
	halfKnown.UserS = f(100)
	if got := TypicalCPUS(append(window, halfKnown)); got != 0 {
		t.Errorf("9 runs with both keys: %v, want 0 (unknown)", got)
	}
	last := run("x", 10)
	last.UserS, last.SysS = f(60), f(4)
	if got := TypicalCPUS(append(window, halfKnown, last)); got != 6.5 {
		t.Errorf("10 runs with both keys: %v, want the median 6.5", got)
	}
}
