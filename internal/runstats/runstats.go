//go:build unix

// Package runstats summarizes the run log per label: what each label needs, learned from its window.
package runstats

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
)

// WindowRuns is how many of a label's latest counted runs its need rests on.
const WindowRuns = 20

// Stats is what the run log says about one label.
type Stats struct {
	Label      string
	Runs       int               // every counted run of the label
	Window     []store.RunRecord // its last WindowRuns, oldest first
	MemPeakKiB int64             // the window's largest peak, 0 when no run has one
	NeedMiB    int64             // the need learned from the window (NeedMiB), 0 when none was measured
	// PeakNeedMiB is the need learned from the window's peaks alone (PeakNeedMiB), 0 when no run
	// has a peak: what a stopped run comes back with, and the pin stats suggests.
	PeakNeedMiB int64
	WaitedS     float64 // summed over the window
	// WindowS is the label's reservation window (WindowS), and WindowLearned whether the window's
	// runs gave it rather than the default.
	WindowS       int64
	WindowLearned bool
}

// counted reports whether r says what its label needs: labelled, not run without the budget, not
// stopped.
func counted(r store.RunRecord) bool {
	return r.Label != "" && r.FailOpen == "" && r.Stops == 0
}

// Window is the last n counted runs of label by time, oldest first.
func Window(runs []store.RunRecord, label string, n int) []store.RunRecord {
	var rs []store.RunRecord
	for _, r := range runs {
		if r.Label == label && counted(r) {
			rs = append(rs, r)
		}
	}
	slices.SortStableFunc(rs, func(a, b store.RunRecord) int { return cmp.Compare(a.TimeNS, b.TimeNS) })
	return rs[max(len(rs)-n, 0):]
}

// SustainCapKiB is how far under its peak a run's figure may fall: 1 GiB, in KiB.
const SustainCapKiB = 1 << 20

// NeedMiB is what the window says its label needs: the p90 of its runs' figures, rounded up to a
// whole MiB and at least 1. A run's figure is its sustained level, never above its peak nor more
// than SustainCapKiB under it; the peak for a run without a sustained level; none for a run
// without a peak. ok is false when no run has a figure.
func NeedMiB(window []store.RunRecord) (mib int64, ok bool) {
	mib, ok = p90MiB(window, func(r store.RunRecord) int64 {
		p := *r.MemPeakKiB
		if r.MemSustainedKiB == nil {
			return p
		}
		return max(min(*r.MemSustainedKiB, p), p-SustainCapKiB)
	})
	if !ok {
		return 0, false
	}
	return max(1, mib), true
}

// PeakNeedMiB is the p90 of the window's memory peaks, rounded up to a whole MiB; ok is false when
// no run measured one.
func PeakNeedMiB(window []store.RunRecord) (mib int64, ok bool) {
	return p90MiB(window, func(r store.RunRecord) int64 { return *r.MemPeakKiB })
}

// p90MiB is the p90 of figure over the window's runs that have a peak, in KiB, rounded up to a
// whole MiB; ok is false when none has one.
func p90MiB(window []store.RunRecord, figure func(store.RunRecord) int64) (mib int64, ok bool) {
	var figs []int64
	for _, r := range window {
		if r.MemPeakKiB != nil {
			figs = append(figs, figure(r))
		}
	}
	if len(figs) == 0 {
		return 0, false
	}
	slices.Sort(figs)
	return (figs[min(len(figs)-1, len(figs)*9/10)] + 1023) / 1024, true
}

// TypicalS is the label's typical run time: the p90 of the window's ran_s, rounded up to a whole
// second and at least 1; 0 when the window is empty.
func TypicalS(window []store.RunRecord) int64 {
	if len(window) == 0 {
		return 0
	}
	ran := make([]float64, 0, len(window))
	for _, r := range window {
		ran = append(ran, r.RanS)
	}
	slices.Sort(ran)
	return max(1, int64(math.Ceil(ran[min(len(ran)-1, len(ran)*9/10)])))
}

// WindowS is the label's reservation window in seconds (PROTOCOL.md section 20): with at least
// state.TailMinWindows runs of the window carrying a counted mem_window_s, 1.5 times the largest,
// rounded up, at least floorS and at most state.TailMaxWindowS; defaultS otherwise. A key counts
// only when it is a number in (0, state.TailMaxWindowS]; any other is ignored as if absent, so a
// forged or corrupt value never sizes the window. learned is false when the default is returned.
func WindowS(window []store.RunRecord, floorS, defaultS int64) (s int64, learned bool) {
	keyed, largest := 0, 0.0
	for _, r := range window {
		if w := r.MemWindowS; w != nil && *w > 0 && *w <= state.TailMaxWindowS { // NaN fails both
			keyed, largest = keyed+1, max(largest, *w)
		}
	}
	if keyed < state.TailMinWindows {
		return defaultS, false
	}
	s = int64(math.Ceil(largest * state.TailFactorNum / state.TailFactorDen))
	return min(state.TailMaxWindowS, max(floorS, s)), true
}

// MinCPURuns is how many runs of a label's window must report their CPU time before a red stop
// round may keep its job as nearly finished.
const MinCPURuns = 10

// TypicalCPUS is the label's typical CPU time: the median of user_s + sys_s over the window's runs
// that report both; 0 when fewer than MinCPURuns do.
func TypicalCPUS(window []store.RunRecord) float64 {
	var cpu []float64
	for _, r := range window {
		if r.UserS != nil && r.SysS != nil {
			cpu = append(cpu, *r.UserS+*r.SysS)
		}
	}
	if len(cpu) < MinCPURuns {
		return 0
	}
	slices.Sort(cpu)
	n := len(cpu)
	return (cpu[(n-1)/2] + cpu[n/2]) / 2
}

// NormalShare is the median of the window's tick shares, the live measure's own definition; ok is
// false when no run has one.
func NormalShare(window []store.RunRecord) (share float64, ok bool) {
	var shares []float64
	for _, r := range window {
		if r.TickShare != nil {
			shares = append(shares, *r.TickShare)
		}
	}
	if len(shares) == 0 {
		return 0, false
	}
	slices.Sort(shares)
	n := len(shares)
	return (shares[(n-1)/2] + shares[n/2]) / 2, true
}

// Summarize groups the counted runs by label and summarizes each, sorted by label.
func Summarize(runs []store.RunRecord) []Stats {
	n := map[string]int{}
	for _, r := range runs {
		if counted(r) {
			n[r.Label]++
		}
	}
	out := make([]Stats, 0, len(n))
	for label, count := range n {
		out = append(out, summarize(label, count, Window(runs, label, WindowRuns)))
	}
	slices.SortFunc(out, func(a, b Stats) int { return strings.Compare(a.Label, b.Label) })
	return out
}

func summarize(label string, n int, window []store.RunRecord) Stats {
	s := Stats{Label: label, Runs: n, Window: window}
	for _, r := range window {
		s.WaitedS += r.WaitedS
		if r.MemPeakKiB != nil {
			s.MemPeakKiB = max(s.MemPeakKiB, *r.MemPeakKiB)
		}
	}
	s.NeedMiB, _ = NeedMiB(window)
	s.PeakNeedMiB, _ = PeakNeedMiB(window)
	s.WindowS, s.WindowLearned = WindowS(window, state.TailFloorS, state.TailDefaultS)
	return s
}

// Span is the window, such as "5 runs 2026-10-20..21". Dates are UTC.
func (s Stats) Span() string {
	return fmt.Sprintf("%d %s %s", len(s.Window), plural(len(s.Window), "run", "runs"),
		span(s.Window[0].TimeNS, s.Window[len(s.Window)-1].TimeNS))
}

// span is the dates from first to last: "2026-10-20", "2026-10-20..21", "2026-10-30..11-02" or
// "2026-12-30..2027-01-02".
func span(first, last int64) string {
	a, b := time.Unix(0, first).UTC(), time.Unix(0, last).UTC()
	from := a.Format(time.DateOnly)
	switch {
	case a.Format(time.DateOnly) == b.Format(time.DateOnly):
		return from
	case a.Year() == b.Year() && a.Month() == b.Month():
		return from + ".." + b.Format("02")
	case a.Year() == b.Year():
		return from + ".." + b.Format("01-02")
	}
	return from + ".." + b.Format(time.DateOnly)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
