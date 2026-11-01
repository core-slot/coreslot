//go:build unix

// Package runstats summarizes the run log per label and suggests what each label needs.
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

// WindowRuns is how many of a label's latest counted runs a suggestion rests on.
const WindowRuns = 5

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

// Suggestion is what the window suggests for the label's needs entry. MemMiB is 0 when no window
// run measured a peak.
type Suggestion struct {
	MemMiB int64
	Reason string
}

// counted reports whether r says what its label needs: labelled, not a pass-through, not run
// without the budget.
func counted(r store.RunRecord) bool {
	return r.Label != "" && !r.Nested && r.FailOpen == ""
}

// Summarize groups the counted runs by label and summarizes each, sorted by label.
func Summarize(runs []store.RunRecord) []Stats {
	byLabel := map[string][]store.RunRecord{}
	for _, r := range runs {
		if counted(r) {
			byLabel[r.Label] = append(byLabel[r.Label], r)
		}
	}
	out := make([]Stats, 0, len(byLabel))
	for label, rs := range byLabel {
		slices.SortStableFunc(rs, func(a, b store.RunRecord) int { return cmp.Compare(a.TimeNS, b.TimeNS) })
		out = append(out, summarize(label, len(rs), rs[max(len(rs)-WindowRuns, 0):]))
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
	s.Suggest.MemMiB = (s.MemPeakKiB + 1023) / 1024
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
