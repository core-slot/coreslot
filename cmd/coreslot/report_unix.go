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
	waitedFromS   = 1.0                  // an attempt waited when its waited_s is at least this
	heaviestShown = 5                    // labels the text lists; --json lists all
	maxRecordS    = 365 * 24 * 60 * 60.0 // a waited_s or ran_s above a year is not a measurement
)

// notMeasuredLines is the text's fixed reminder that the report has no counterfactual.
var notMeasuredLines = []string{
	"what these runs would have done without coreslot is not measured here;",
	"results measured on one 16 GB MacBook Air, with and without room, are in",
	"https://github.com/core-slot/coreslot/blob/main/evidence/air-v04/README.md",
}

// report is what `coreslot report` prints, and its --json document. An attempt is a run-log
// line; a run is a line without stops, or a give-up. SkippedLines counts the lines that did not
// decode, lie in the future or hold an impossible value.
type report struct {
	Schema       int           `json:"schema"`
	Days         int           `json:"days"`
	SinceNS      int64         `json:"since_ns"`
	UntilNS      int64         `json:"until_ns"`
	SkippedLines int           `json:"skipped_lines"`
	Runs         int           `json:"runs"`
	Attempts     int           `json:"attempts"`
	FailOpenRuns int           `json:"fail_open_runs"` // runs outside the budget
	Labels       int           `json:"labels"`
	Waits        reportWaits   `json:"waits"`
	Stops        reportStops   `json:"stops"`
	Heaviest     []reportLabel `json:"heaviest"`
}

// reportWaits are over the attempts that waited at least waitedFromS.
type reportWaits struct {
	Attempts  int     `json:"attempts"`
	TotalS    float64 `json:"total_s"`
	MedianS   float64 `json:"median_s"`
	P90S      float64 `json:"p90_s"`
	LongestS  float64 `json:"longest_s"`
	Over2Min  int     `json:"over_2_min"`  // waited_s > 120
	Over10Min int     `json:"over_10_min"` // waited_s > 600
}

type reportStops struct {
	StoppedAttempts int     `json:"stopped_attempts"` // attempts stopped for memory
	LostS           float64 `json:"lost_s"`           // the ran_s of those attempts
	GaveUpRuns      int     `json:"gave_up_runs"`     // runs that gave up after maxStops stops
}

// reportLabel is one label in the window. PeakMiB, RanS and WaitedS cover every attempt, stopped
// ones included; NeedMiB is the need coreslot learned from the whole log, as stats prints it.
// PeakMiB and NeedMiB are null when unknown.
type reportLabel struct {
	Label    string  `json:"label"`
	Runs     int     `json:"runs"`
	Attempts int     `json:"attempts"`
	PeakMiB  *int64  `json:"peak_mib"`
	NeedMiB  *int64  `json:"need_mib"`
	RanS     float64 `json:"ran_s"`
	WaitedS  float64 `json:"waited_s"`
	// Container says an attempt in the window was container work: the peak and the need are its
	// client's alone.
	Container bool `json:"container,omitempty"`
}

// plausible reports whether r can be a measurement taken before now.
func plausible(r store.RunRecord, now time.Time) bool {
	return r.TimeNS > 0 && r.TimeNS <= now.UnixNano() && r.Stops >= 0 &&
		r.WaitedS >= 0 && r.WaitedS <= maxRecordS && r.RanS >= 0 && r.RanS <= maxRecordS &&
		(r.MemPeakKiB == nil || *r.MemPeakKiB >= 0)
}

// buildReport reports on the attempts that started in the days before now. all is the whole log,
// from which the learned needs come; a line that is not plausible counts as skipped.
func buildReport(all []store.RunRecord, skipped int, now time.Time, days int) report {
	since := now.Add(-time.Duration(days) * 24 * time.Hour)
	rep := report{Schema: reportSchema, Days: days, SinceNS: since.UnixNano(), UntilNS: now.UnixNano(), Heaviest: []reportLabel{}}
	var valid, in []store.RunRecord
	for _, r := range all {
		switch {
		case !plausible(r, now):
			skipped++
		case r.TimeNS >= rep.SinceNS:
			in = append(in, r)
			fallthrough
		default:
			valid = append(valid, r)
		}
	}
	rep.SkippedLines = skipped
	needs := map[string]int64{}
	for _, s := range runstats.Summarize(valid) {
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
		if r.FailOpen != "" { // never stopped: a run outside the budget ticks nothing
			rep.FailOpenRuns++
		}
		if r.WaitedS >= waitedFromS {
			waited = append(waited, r.WaitedS)
		}
		if r.Stops > 0 {
			rep.Stops.StoppedAttempts++
			rep.Stops.LostS += r.RanS
			if r.Stops == maxStops {
				rep.Stops.GaveUpRuns++
			}
		}
		h := byLabel[r.Label]
		if h == nil {
			h = &reportLabel{Label: r.Label}
			if n := needs[r.Label]; n > 0 {
				h.NeedMiB = new(n)
			}
			byLabel[r.Label] = h
		}
		h.Attempts++
		if isRun {
			h.Runs++
		}
		h.RanS += r.RanS
		h.WaitedS += r.WaitedS
		h.Container = h.Container || r.Container
		if p := r.MemPeakKiB; p != nil && (h.PeakMiB == nil || (*p+1023)/1024 > *h.PeakMiB) {
			h.PeakMiB = new((*p + 1023) / 1024)
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
	return rep
}

func peakOf(h reportLabel) int64 {
	if h.PeakMiB == nil {
		return -1
	}
	return *h.PeakMiB
}

func waitsOf(w []float64) reportWaits {
	out := reportWaits{Attempts: len(w)}
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
	n := len(w)
	out.MedianS, out.P90S, out.LongestS = (w[(n-1)/2]+w[n/2])/2, w[min(n-1, n*9/10)], w[n-1]
	return out
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
		r := fmt.Sprintf("%d in %d %s (%d %s", rep.Runs, rep.Labels, plural(int64(rep.Labels), "label", "labels"),
			rep.Attempts, plural(int64(rep.Attempts), "attempt", "attempts"))
		if rep.FailOpenRuns > 0 {
			r += fmt.Sprintf("; %d %s outside the budget", rep.FailOpenRuns, plural(int64(rep.FailOpenRuns), "run", "runs"))
		}
		line("runs", "%s)", r)
		wt := rep.Waits
		if wt.Attempts == 0 {
			line("waits", "no attempt waited %s or more", secs(waitedFromS))
		} else {
			line("waits", "%d %s waited %s or more: total %s, median %s, p90 %s, longest %s", wt.Attempts,
				plural(int64(wt.Attempts), "attempt", "attempts"), secs(waitedFromS),
				secs(wt.TotalS), secs(wt.MedianS), secs(wt.P90S), secs(wt.LongestS))
			line("", "%d waited over 2 min, %d over 10 min", wt.Over2Min, wt.Over10Min)
		}
		st := rep.Stops
		if st.StoppedAttempts == 0 {
			line("stops", "none")
		} else {
			line("stops", "%d %s stopped for memory, %s of run time lost to stops", st.StoppedAttempts,
				plural(int64(st.StoppedAttempts), "attempt", "attempts"), secs(st.LostS))
			line("gave up", "%d %s, after %d stops", st.GaveUpRuns, plural(int64(st.GaveUpRuns), "run", "runs"), maxStops)
		}
	}
	if len(rep.Heaviest) > 0 {
		b.WriteString("heaviest labels: peak, ran and waited cover every attempt, stopped ones included;\n" +
			"need is what coreslot learned from the whole log\n")
		w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  label\truns\tattempts\tpeak\tneed\tran\twaited")
		mib := func(p *int64) string {
			if p == nil {
				return "-"
			}
			return spec.FormatMiB(*p)
		}
		var containers []string
		for _, h := range rep.Heaviest[:min(len(rep.Heaviest), heaviestShown)] {
			fmt.Fprintf(w, "  %s\t%d\t%d\t%s\t%s\t%s\t%s\n", labelText(h.Label), h.Runs, h.Attempts,
				mib(h.PeakMiB), mib(h.NeedMiB), secs(h.RanS), secs(h.WaitedS))
			if h.Container {
				containers = append(containers, labelText(h.Label))
			}
		}
		w.Flush()
		if n := len(rep.Heaviest) - heaviestShown; n > 0 {
			fmt.Fprintf(&b, "  and %d more (--json lists all)\n", n)
		}
		if len(containers) > 0 {
			fmt.Fprintf(&b, "  container labels: %s (peak and need are the client's)\n", strings.Join(containers, ", "))
		}
	}
	for i, l := range notMeasuredLines {
		key := ""
		if i == 0 {
			key = "not measured"
		}
		line(key, "%s", l)
	}
	if rep.SkippedLines > 0 {
		line("skipped", "%d run-log %s: undecodable, in the future or holding an impossible value",
			rep.SkippedLines, plural(int64(rep.SkippedLines), "line", "lines"))
	}
	return b.String()
}
