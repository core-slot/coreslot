//go:build unix

package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/core-slot/coreslot/internal/runstats"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// reportSchema is the version of the `report --json` document.
const reportSchema = 1

const (
	waitedFromS    = 1.0 // an attempt waited when its waited_s is at least this
	sharingMinRuns = 10  // runs a label needs both alone and alongside others for a sharing cost
	heaviestShown  = 5   // labels the text lists; --json lists all
)

// notMeasuredLines is the report's fixed reminder that it has no counterfactual; the JSON
// document carries it as one sentence (notMeasured).
var notMeasuredLines = []string{
	"what these runs would have done without coreslot; on a different machine,",
	"a 16 GB MacBook Air with four agents, the agents' mean time to a verdict",
	"under coreslot was 0.40 of unwrapped:",
	"https://github.com/core-slot/coreslot/blob/main/evidence/air-v04/README.md",
}

var notMeasured = strings.Join(notMeasuredLines, " ")

const sharingCaveat = "estimate; caches and load outside coreslot not controlled"

// report is what `coreslot report` prints, and its --json document. A run is a run-log line
// without stops or a give-up; an attempt is any line, a stopped one included.
type report struct {
	Schema       int           `json:"schema"`
	Days         int           `json:"days"`
	SinceNS      int64         `json:"since_ns"`
	UntilNS      int64         `json:"until_ns"`
	SkippedLines int           `json:"skipped_lines"`
	Runs         int           `json:"runs"`
	Attempts     int           `json:"attempts"`
	FailOpen     int           `json:"fail_open"` // attempts that ran outside the budget
	Labels       int           `json:"labels"`
	Waits        reportWaits   `json:"waits"`
	Stops        reportStops   `json:"stops"`
	Heaviest     []reportLabel `json:"heaviest"`
	Sharing      reportSharing `json:"sharing_cost"`
	NotMeasured  string        `json:"not_measured"`
}

// waits are over the attempts that waited at least waitedFromS.
type reportWaits struct {
	Waited    int     `json:"waited"`
	TotalS    float64 `json:"total_s"`
	MedianS   float64 `json:"median_s"`
	P90S      float64 `json:"p90_s"`
	LongestS  float64 `json:"longest_s"`
	Over2Min  int     `json:"over_2_min"`
	Over10Min int     `json:"over_10_min"`
}

type reportStops struct {
	Stopped int     `json:"stopped"`  // attempts stopped for memory
	RedoneS float64 `json:"redone_s"` // their ran_s: work run again
	GaveUp  int     `json:"gave_up"`  // runs that gave up after maxStops stops
}

// reportLabel is one label in the window. MemPeakKiB is the largest peak of its attempts; Need is the
// need coreslot learned for it from the whole log, as stats prints it. Both are null when unknown.
type reportLabel struct {
	Label      string  `json:"label"`
	Runs       int     `json:"runs"`
	MemPeakKiB *int64  `json:"mem_peak_kib"`
	Need       *string `json:"need"`
	RanS       float64 `json:"ran_s"`
	WaitedS    float64 `json:"waited_s"`
}

// sharing is the estimate: per label, the median ran_s of its counted runs that overlapped
// another coreslot attempt over the median of those that ran alone.
type reportSharing struct {
	Estimate string       `json:"estimate"`
	MinRuns  int          `json:"min_runs"`
	TooFew   int          `json:"labels_too_few"` // labels in the window below MinRuns either way
	Labels   []shareLabel `json:"labels"`
}

type shareLabel struct {
	Label         string  `json:"label"`
	Shared        int     `json:"shared"`
	Alone         int     `json:"alone"`
	SharedMedianS float64 `json:"shared_median_s"`
	AloneMedianS  float64 `json:"alone_median_s"`
	Ratio         float64 `json:"ratio"`
}

// runSpan is an attempt's run on the wall clock: it starts when its wait ends.
type runSpan struct{ start, end float64 }

func spanOf(r store.RunRecord) runSpan {
	s := float64(r.TimeNS)/1e9 + r.WaitedS
	return runSpan{s, s + r.RanS}
}

// buildReport reports on the attempts that started in the days before now. all is the whole log:
// the learned needs and the overlaps read it beyond the window.
func buildReport(all []store.RunRecord, skipped int, now time.Time, days int) report {
	since := now.Add(-time.Duration(days) * 24 * time.Hour)
	rep := report{Schema: reportSchema, Days: days, SinceNS: since.UnixNano(), UntilNS: now.UnixNano(),
		SkippedLines: skipped, Heaviest: []reportLabel{}, NotMeasured: notMeasured,
		Sharing: reportSharing{Estimate: sharingCaveat, MinRuns: sharingMinRuns, Labels: []shareLabel{}}}
	var in []store.RunRecord
	for _, r := range all {
		if r.TimeNS >= rep.SinceNS {
			in = append(in, r)
		}
	}
	needs := map[string]int64{}
	for _, s := range runstats.Summarize(all) {
		needs[s.Label] = s.NeedMiB
	}
	byLabel := map[string]*reportLabel{}
	var waited []float64
	for _, r := range in {
		rep.Attempts++
		isRun := r.Stops == 0 || r.Stops == maxStops
		if isRun {
			rep.Runs++
		}
		if r.FailOpen != "" {
			rep.FailOpen++
		}
		if r.WaitedS >= waitedFromS {
			waited = append(waited, r.WaitedS)
		}
		if r.Stops > 0 {
			rep.Stops.Stopped++
			rep.Stops.RedoneS += r.RanS
			if r.Stops == maxStops {
				rep.Stops.GaveUp++
			}
		}
		h := byLabel[r.Label]
		if h == nil {
			h = &reportLabel{Label: r.Label}
			if n := needs[r.Label]; n > 0 {
				h.Need = new(spec.NeedText(n))
			}
			byLabel[r.Label] = h
		}
		if isRun {
			h.Runs++
		}
		h.RanS += r.RanS
		h.WaitedS += r.WaitedS
		if p := r.MemPeakKiB; p != nil && (h.MemPeakKiB == nil || *p > *h.MemPeakKiB) {
			h.MemPeakKiB = new(*p)
		}
	}
	rep.Labels = len(byLabel)
	rep.Waits = waitsOf(waited)
	for _, h := range byLabel {
		rep.Heaviest = append(rep.Heaviest, *h)
	}
	slices.SortFunc(rep.Heaviest, func(a, b reportLabel) int {
		return cmp.Or(-cmp.Compare(peakOf(a), peakOf(b)), -cmp.Compare(a.RanS, b.RanS), strings.Compare(a.Label, b.Label))
	})
	rep.Sharing.Labels, rep.Sharing.TooFew = sharingOf(all, in)
	return rep
}

func peakOf(h reportLabel) int64 {
	if h.MemPeakKiB == nil {
		return -1
	}
	return *h.MemPeakKiB
}

func waitsOf(w []float64) reportWaits {
	out := reportWaits{Waited: len(w)}
	if len(w) == 0 {
		return out
	}
	slices.Sort(w)
	for _, s := range w {
		out.TotalS += s
		if s > 120 {
			out.Over2Min++
		}
		if s > 600 {
			out.Over10Min++
		}
	}
	out.MedianS, out.P90S, out.LongestS = median(w), w[min(len(w)-1, len(w)*9/10)], w[len(w)-1]
	return out
}

// median of sorted xs, which is not empty.
func median(xs []float64) float64 {
	n := len(xs)
	return (xs[(n-1)/2] + xs[n/2]) / 2
}

// sharingOf splits each label's counted runs in the window (in) into those whose run overlapped
// another attempt of the whole log (all) and those that ran alone, and returns the labels with
// sharingMinRuns of each, by label, and how many labels fell short.
func sharingOf(all, in []store.RunRecord) ([]shareLabel, int) {
	spans := make([]runSpan, len(all))
	for i, r := range all {
		spans[i] = spanOf(r)
	}
	slices.SortFunc(spans, func(a, b runSpan) int { return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(a.end, b.end)) })
	// top[i] holds the two latest ends among spans[:i], so that one of them is not the run's own.
	top := make([][2]float64, len(spans)+1)
	top[0] = [2]float64{-1, -1}
	for i, s := range spans {
		t := top[i]
		switch {
		case s.end >= t[0]:
			t = [2]float64{s.end, t[0]}
		case s.end > t[1]:
			t[1] = s.end
		}
		top[i+1] = t
	}
	shared, alone := map[string][]float64{}, map[string][]float64{}
	labels := map[string]bool{}
	for _, r := range in {
		if r.Label == "" || r.FailOpen != "" || r.Stops > 0 { // runstats' counted runs
			continue
		}
		labels[r.Label] = true
		s := spanOf(r)
		// The spans that start before s ends: s itself is among them when it has a length.
		k, _ := slices.BinarySearchFunc(spans, s.end, func(x runSpan, end float64) int {
			if x.start < end {
				return -1
			}
			return 1
		})
		latest := top[k][0]
		if s.end > s.start && latest == s.end {
			latest = top[k][1] // s's own end, or a tie with another's: the runner-up decides
		}
		if latest > s.start {
			shared[r.Label] = append(shared[r.Label], r.RanS)
		} else {
			alone[r.Label] = append(alone[r.Label], r.RanS)
		}
	}
	out := []shareLabel{}
	tooFew := 0
	for label := range labels {
		sh, al := shared[label], alone[label]
		if len(sh) < sharingMinRuns || len(al) < sharingMinRuns {
			tooFew++
			continue
		}
		slices.Sort(sh)
		slices.Sort(al)
		l := shareLabel{Label: label, Shared: len(sh), Alone: len(al), SharedMedianS: median(sh), AloneMedianS: median(al)}
		if l.AloneMedianS <= 0 {
			tooFew++
			continue
		}
		l.Ratio = l.SharedMedianS / l.AloneMedianS
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b shareLabel) int { return strings.Compare(a.Label, b.Label) })
	return out, tooFew
}

// reportCommand is `coreslot report [--days N] [--json]`: what the run log says coreslot did in
// the last N days. It exits 1 when the state directory or the run log cannot be read.
func reportCommand(args []string, io IO) int {
	fs, days, asJSON := reportFlags()
	if code, ok := parseOnly(fs, args, "report", reportUsage, io); !ok {
		return code
	}
	if *days < 1 || *days > maxReportDays {
		fmt.Fprintf(io.Stderr, "coreslot: report: --days must be between 1 and %d\n%s\n", maxReportDays, reportUsage)
		return exitUsage
	}
	runs, skipped, _, err := readRunLog()
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: report: %s\n", text(err.Error()))
		return exitProblems
	}
	rep := buildReport(runs, skipped, time.Now(), *days)
	if *asJSON {
		if err := writeReportJSON(io, rep); err != nil {
			fmt.Fprintf(io.Stderr, "coreslot: report: %s\n", text(err.Error()))
			return exitInternal
		}
		return 0
	}
	fmt.Fprint(io.Stdout, rep.human())
	return 0
}

func writeReportJSON(io IO, rep report) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return err
	}
	_, err := io.Stdout.Write(wire.EscapeJSON(buf.Bytes()))
	return err
}

func secs(s float64) string { return duration(time.Duration(s * float64(time.Second))) }

func (rep report) human() string {
	var b strings.Builder
	line := func(key, format string, args ...any) {
		fmt.Fprintf(&b, "%-12s  "+format+"\n", append([]any{key}, args...)...)
	}
	line("window", "last %d %s, since %s", rep.Days, plural(int64(rep.Days), "day", "days"),
		time.Unix(0, rep.SinceNS).UTC().Format("2006-01-02 15:04 UTC"))
	if rep.Attempts == 0 {
		line("runs", "none recorded in the window")
	} else {
		r := fmt.Sprintf("%d in %d %s, %d %s", rep.Runs, rep.Labels, plural(int64(rep.Labels), "label", "labels"),
			rep.Attempts, plural(int64(rep.Attempts), "attempt", "attempts"))
		if rep.FailOpen > 0 {
			r += fmt.Sprintf(", %d outside the budget", rep.FailOpen)
		}
		line("runs", "%s", r)
		wt := rep.Waits
		if wt.Waited == 0 {
			line("waits", "no attempt waited %s or more", secs(waitedFromS))
		} else {
			line("waits", "%d %s waited %s or more: total %s, median %s, p90 %s, longest %s", wt.Waited,
				plural(int64(wt.Waited), "attempt", "attempts"), secs(waitedFromS),
				secs(wt.TotalS), secs(wt.MedianS), secs(wt.P90S), secs(wt.LongestS))
			line("", "%d over 2 min, %d over 10 min", wt.Over2Min, wt.Over10Min)
		}
		st := rep.Stops
		if st.Stopped == 0 {
			line("stops", "none")
		} else {
			line("stops", "%d %s stopped for memory, %s of work redone, %d gave up after %d stops", st.Stopped,
				plural(int64(st.Stopped), "attempt", "attempts"), secs(st.RedoneS), st.GaveUp, maxStops)
		}
	}
	if len(rep.Heaviest) > 0 {
		b.WriteString("heaviest labels (peak in the window; need as learned from the whole log)\n")
		w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  label\truns\tpeak\tneed\tran\twaited")
		for _, h := range rep.Heaviest[:min(len(rep.Heaviest), heaviestShown)] {
			peak, need := "-", "-"
			if h.MemPeakKiB != nil {
				peak = spec.FormatMiB((*h.MemPeakKiB + 1023) / 1024)
			}
			if h.Need != nil {
				need = *h.Need
			}
			fmt.Fprintf(w, "  %s\t%d\t%s\t%s\t%s\t%s\n", labelText(h.Label), h.Runs, peak, need, secs(h.RanS), secs(h.WaitedS))
		}
		w.Flush()
		if n := len(rep.Heaviest) - heaviestShown; n > 0 {
			fmt.Fprintf(&b, "  and %d more (--json lists all)\n", n)
		}
	}
	sh := rep.Sharing
	if len(sh.Labels) == 0 {
		line("estimate", "sharing cost: no label has %d runs both alone and alongside other coreslot runs", sh.MinRuns)
	}
	for i, l := range sh.Labels {
		key := ""
		if i == 0 {
			key = "estimate"
		}
		line(key, "sharing cost of %s: %.2fx, the median run %s alongside other coreslot runs (%d) against %s alone (%d)",
			labelText(l.Label), l.Ratio, secs(l.SharedMedianS), l.Shared, secs(l.AloneMedianS), l.Alone)
	}
	if len(sh.Labels) > 0 {
		line("", "(%s)", sh.Estimate)
	}
	for i, l := range notMeasuredLines {
		key := ""
		if i == 0 {
			key = "not measured"
		}
		line(key, "%s", l)
	}
	if rep.SkippedLines > 0 {
		fmt.Fprintf(&b, "skipped %d run-log %s that did not decode\n", rep.SkippedLines, plural(int64(rep.SkippedLines), "line", "lines"))
	}
	return b.String()
}
