//go:build unix

// Package runstats summarizes the run log per label and suggests what each label needs.
package runstats

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/core-slot/coreslot/internal/needs"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
)

const (
	// WindowRuns is how many of a label's latest counted runs a suggestion rests on.
	WindowRuns = 5
	// saturatedShare is the share of its cpu grant a run must use to count as saturated.
	saturatedShare = 0.8
)

// Stats is what the run log says about one base label (needs.BaseLabel).
type Stats struct {
	Label         string
	Runs          int               // every counted run of the label
	Window        []store.RunRecord // its last WindowRuns, oldest first
	CoresUsed     []float64         // per window run with its CPU time and ran_s > 0, in order
	Saturated     int               // window runs that used saturatedShare of their cpu grant or more
	MemPeakKiB    int64             // the window's largest peak, 0 when no run has one
	MemPeakSource string            // that peak's measure
	PeakAfterS    *float64          // when that peak was read, nil when unknown
	PeakCPU       int               // that run's cpu grant when above the suggested cpu max, else 0
	WaitedS       float64           // summed over the window
	WaitedFreeS   float64           // summed over the window; a run without it adds 0
	Suggest       Suggestion
}

// Suggestion is what the window suggests for the label's needs entry. CPU is nil and MemMiB 0
// when no window run measured them; CPUPerWorker, the cores one worker uses, is 0 when no
// measured window run carries workers.
type Suggestion struct {
	CPU          *spec.Range
	MemMiB       int64
	CPUPerWorker float64
	Reason       string
}

// counted reports whether r says what its label needs: labelled, not a pass-through, not run
// without the budget.
func counted(r store.RunRecord) bool {
	return r.Label != "" && !r.Nested && r.FailOpen == ""
}

// Summarize groups the counted runs by base label and summarizes each, sorted by label. cpuSize
// is the cpu resource's size, which clamps the cpu suggestion.
func Summarize(runs []store.RunRecord, cpuSize int) []Stats {
	byLabel := map[string][]store.RunRecord{}
	for _, r := range runs {
		if base := needs.BaseLabel(r.Label); counted(r) && base != "" {
			byLabel[base] = append(byLabel[base], r)
		}
	}
	out := make([]Stats, 0, len(byLabel))
	for label, rs := range byLabel {
		slices.SortStableFunc(rs, func(a, b store.RunRecord) int { return cmp.Compare(a.TimeNS, b.TimeNS) })
		out = append(out, summarize(label, len(rs), rs[max(len(rs)-WindowRuns, 0):], cpuSize))
	}
	slices.SortFunc(out, func(a, b Stats) int { return strings.Compare(a.Label, b.Label) })
	return out
}

func summarize(label string, n int, window []store.RunRecord, cpuSize int) Stats {
	s := Stats{Label: label, Runs: n, Window: window}
	maxCores := 0.0
	var perWorker []float64
	for _, r := range window {
		s.WaitedS += r.WaitedS
		if r.WaitedFreeS != nil {
			s.WaitedFreeS += *r.WaitedFreeS
		}
		if r.CPUUserS == nil || r.CPUSysS == nil || r.RanS <= 0 {
			continue
		}
		cores := (*r.CPUUserS + *r.CPUSysS) / r.RanS
		s.CoresUsed = append(s.CoresUsed, cores)
		maxCores = max(maxCores, cores)
		if r.Workers != nil && *r.Workers > 0 {
			perWorker = append(perWorker, cores/float64(*r.Workers))
		}
		if g := r.Grant["cpu"]; g > 0 && cores >= saturatedShare*float64(g) {
			s.Saturated++
		}
	}
	if len(perWorker) > 0 {
		// The p90, rounded up to a multiple of 0.05; the epsilon keeps a product that float
		// arithmetic leaves a hair above a multiple from rounding up to the next. coreslot never
		// caps a run's cores, so workers that want more than their share show it here.
		slices.Sort(perWorker)
		p90 := perWorker[int(math.Ceil(0.9*float64(len(perWorker))))-1]
		s.Suggest.CPUPerWorker = max(math.Ceil(p90*20-1e-9)/20, 0.05)
	}
	if len(s.CoresUsed) > 0 {
		x := min(max(int(math.Ceil(maxCores)), 1), max(cpuSize, 1), spec.MaxPerResource)
		cpu := spec.Range{Min: x, Max: x}
		if last := window[len(window)-1].Need["cpu"]; last.Min < last.Max {
			cpu.Min = 1
			if s.Suggest.CPUPerWorker > 0 && s.Saturated == 0 {
				// Its workers follow the grant, so it uses fewer cores when granted fewer: narrowing
				// the max to the cores used would narrow it again on every learn.
				cpu.Max = last.Max
			}
		}
		s.Suggest.CPU = &cpu
	}
	s.memPeak(window)
	const tokenKiB = spec.MemTokenMiB * 1024
	s.Suggest.MemMiB = (s.MemPeakKiB + tokenKiB - 1) / tokenKiB * spec.MemTokenMiB
	s.Suggest.Reason = s.reason(maxCores)
	return s
}

// memPeak sets the window's largest peak, and in PeakCPU the cpu grant of its run when that is above
// the suggested cpu max: a command that takes {n} uses memory per worker, so such a peak may not hold
// at the grant the label will get, and the evidence says so rather than guessing that grant.
func (s *Stats) memPeak(window []store.RunRecord) {
	for _, r := range window {
		if r.MemPeakKiB != nil && *r.MemPeakKiB > s.MemPeakKiB {
			s.MemPeakKiB, s.MemPeakSource, s.PeakAfterS = *r.MemPeakKiB, r.MemPeakSource, r.PeakAfterS
			s.PeakCPU = 0
			if g := r.Grant["cpu"]; s.Suggest.CPU != nil && g > s.Suggest.CPU.Max {
				s.PeakCPU = g
			}
		}
	}
}

// reason is the evidence for the suggestion, such as "5 runs 2026-10-20..21: max 5.6 cores, 0 of
// 5 saturated, peak 2.7 GiB (footprint) at 22s". Dates are UTC.
func (s Stats) reason(maxCores float64) string {
	parts := []string{fmt.Sprintf("%d %s %s", len(s.Window), plural(len(s.Window), "run", "runs"),
		span(s.Window[0].TimeNS, s.Window[len(s.Window)-1].TimeNS))}
	if len(s.CoresUsed) > 0 {
		parts = append(parts, fmt.Sprintf("max %.1f cores, %d of %d saturated", maxCores, s.Saturated, len(s.CoresUsed)))
	}
	if s.Suggest.CPUPerWorker > 0 {
		parts = append(parts, fmt.Sprintf("%[1]g cores per worker: set cpu_per_worker = %[1]g", s.Suggest.CPUPerWorker))
	}
	if s.MemPeakKiB > 0 {
		peak := "peak " + spec.FormatMiB((s.MemPeakKiB+1023)/1024)
		if s.MemPeakSource != "" {
			peak += " (" + s.MemPeakSource + ")"
		}
		if s.PeakAfterS != nil {
			peak += fmt.Sprintf(" at %.0fs", *s.PeakAfterS)
		}
		if s.PeakCPU > 0 {
			peak += fmt.Sprintf(" with cpu %d", s.PeakCPU)
		}
		parts = append(parts, peak)
	}
	head, rest := parts[0], parts[1:]
	if len(rest) == 0 {
		return head
	}
	return head + ": " + strings.Join(rest, ", ")
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
