//go:build unix

// Package runstats summarizes the run log per label and says what each label needs.
package runstats

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
)

// WindowRuns is how many of a label's latest counted runs its need rests on.
const WindowRuns = 20

// Stats is what the run log says about one label.
type Stats struct {
	Label         string
	Runs          int               // every counted run of the label
	Window        []store.RunRecord // its last WindowRuns, oldest first
	MemPeakKiB    int64             // the window's largest peak, 0 when no run has one
	MemPeakSource string            // that peak's measure
	PeakAfterS    *float64          // when that peak was read, nil when unknown
	WaitedS       float64           // summed over the window
	WaitedFreeS   float64           // summed over the window; a run without it adds 0
	Suggest       Suggestion
}

// Suggestion is what the window suggests for the label's needs entry: its need. MemMiB is 0 when
// no window run measured a peak.
type Suggestion struct {
	MemMiB int64
	Reason string
}

// counted reports whether r says what its label needs: labelled, not a pass-through, not run
// without the budget, not stopped.
func counted(r store.RunRecord) bool {
	return r.Label != "" && !r.Nested && r.FailOpen == "" && r.Stops == 0
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

// NeedMiB is the p90 of the window's memory peaks, rounded up to a whole MiB; ok is false when
// no run measured one.
func NeedMiB(window []store.RunRecord) (mib int64, ok bool) {
	var peaks []int64
	for _, r := range window {
		if r.MemPeakKiB != nil {
			peaks = append(peaks, *r.MemPeakKiB)
		}
	}
	if len(peaks) == 0 {
		return 0, false
	}
	slices.Sort(peaks)
	return (peaks[min(len(peaks)-1, len(peaks)*9/10)] + 1023) / 1024, true
}

// NormalShare is the median share of the window's runs' CPU time spent in the system,
// sys_s/(user_s+sys_s); ok is false when no run has a measured, non-zero time.
func NormalShare(window []store.RunRecord) (share float64, ok bool) {
	var shares []float64
	for _, r := range window {
		if r.UserS != nil && r.SysS != nil && *r.UserS+*r.SysS > 0 {
			shares = append(shares, *r.SysS/(*r.UserS+*r.SysS))
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
		if r.WaitedFreeS != nil {
			s.WaitedFreeS += *r.WaitedFreeS
		}
		if r.MemPeakKiB != nil && *r.MemPeakKiB > s.MemPeakKiB {
			s.MemPeakKiB, s.MemPeakSource, s.PeakAfterS = *r.MemPeakKiB, r.MemPeakSource, r.PeakAfterS
		}
	}
	s.Suggest.MemMiB, _ = NeedMiB(window)
	s.Suggest.Reason = s.reason()
	return s
}

// reason is the evidence for the suggestion, such as "5 runs 2026-10-20..21: peak 2.7 GiB
// (footprint) at 22s". Dates are UTC.
func (s Stats) reason() string {
	head := fmt.Sprintf("%d %s %s", len(s.Window), plural(len(s.Window), "run", "runs"),
		span(s.Window[0].TimeNS, s.Window[len(s.Window)-1].TimeNS))
	if s.MemPeakKiB == 0 {
		return head
	}
	peak := "peak " + spec.FormatMiB((s.MemPeakKiB+1023)/1024)
	if s.MemPeakSource != "" {
		peak += " (" + s.MemPeakSource + ")"
	}
	if s.PeakAfterS != nil {
		peak += fmt.Sprintf(" at %.0fs", *s.PeakAfterS)
	}
	return head + ": " + peak
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
