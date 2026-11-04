//go:build unix

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/store"
)

// reportNow is the fixture's "now": testdata/runs-report.jsonl places its runs from six days
// before it, one per hour, with a run at exactly seven days before it and one a nanosecond earlier.
var reportNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// reportDir is a state directory whose run log is testdata/runs-report.jsonl.
func reportDir(t *testing.T) string {
	t.Helper()
	dir := stateDir(t, "")
	data, err := os.ReadFile(filepath.Join("testdata", "runs-report.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "log"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "log", "runs.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// reportFixture is the fixture's records as the store decodes them, with the lines it skipped.
func reportFixture(t *testing.T) ([]store.RunRecord, int) {
	t.Helper()
	st, err := store.OpenReadOnly(reportDir(t))
	if err != nil {
		t.Fatal(err)
	}
	runs, skipped, err := st.ReadRuns()
	if err != nil {
		t.Fatal(err)
	}
	return runs, skipped
}

func kib(n int64) *int64 { return &n }

// wantSeven is the fixture's report over seven days, every figure worked out by hand from the
// fixture: gate:unit ran alone 10 times (10..19 s) and alongside lint 10 times (20..29 s); build
// waited 5, 30, 150, 700 and 0.5 s and once ran outside the budget; e2e was stopped twice and then
// finished, and gave up after its third stop in another run.
func wantSeven() report {
	return report{
		Schema: 1, Days: 7, SinceNS: reportNow.Add(-7 * 24 * time.Hour).UnixNano(), UntilNS: reportNow.UnixNano(),
		SkippedLines: 2,
		Runs:         39, // gate:unit 20, lint 10, build 7, e2e 2 (one finished, one gave up)
		Attempts:     43,
		FailOpen:     1,
		Labels:       4,
		Waits:        reportWaits{Waited: 4, TotalS: 885, MedianS: 90, P90S: 700, LongestS: 700, Over2Min: 2, Over10Min: 1},
		Stops:        reportStops{Stopped: 5, RedoneS: 40 + 50 + 10 + 10 + 10, GaveUp: 1},
		Heaviest: []reportLabel{
			{Label: "e2e", Runs: 2, MemPeakKiB: kib(4100000), Need: new("3809M"), RanS: 180, WaitedS: 1.5},
			{Label: "build", Runs: 7, MemPeakKiB: kib(3000000), Need: new("2930M"), RanS: 420, WaitedS: 886},
			{Label: "gate:unit", Runs: 20, MemPeakKiB: kib(1019000), Need: new("995M"), RanS: 390, WaitedS: 5},
			{Label: "lint", Runs: 10, MemPeakKiB: kib(200000), Need: new("196M"), RanS: 50, WaitedS: 2.5},
		},
		Sharing: reportSharing{Estimate: sharingCaveat, MinRuns: 10, TooFew: 3, // lint, build, e2e
			Labels: []shareLabel{{Label: "gate:unit", Shared: 10, Alone: 10, SharedMedianS: 24.5, AloneMedianS: 14.5, Ratio: 24.5 / 14.5}}},
		NotMeasured: notMeasured,
	}
}

// Every figure of the report, over the fixture and over cuts of it. Mutations: count a wait from
// waited_s > 1 instead of >= 1 (the exactly-one-second case);
// keep the run's own span among its overlaps (every gate:unit run is shared, so
// no label qualifies);
// filter the window with > instead of >= (the run at exactly seven days drops: Runs 38); count a
// stopped attempt as a run (Runs 43); drop the malformed-line count (SkippedLines 0).
func TestBuildReport(t *testing.T) {
	runs, skipped := reportFixture(t)
	if skipped != 2 || len(runs) != 45 {
		t.Fatalf("fixture: %d records, %d skipped; want 45, 2", len(runs), skipped)
	}
	empty := func(days int, skipped int) report {
		return report{Schema: 1, Days: days, SinceNS: reportNow.Add(-time.Duration(days) * 24 * time.Hour).UnixNano(),
			UntilNS: reportNow.UnixNano(), SkippedLines: skipped, Heaviest: []reportLabel{}, NotMeasured: notMeasured,
			Sharing: reportSharing{Estimate: sharingCaveat, MinRuns: 10, Labels: []shareLabel{}}}
	}
	cases := []struct {
		name    string
		runs    []store.RunRecord
		skipped int
		days    int
		want    report
	}{
		{"seven days", runs, skipped, 7, wantSeven()},
		{"six days cut the run at the edge", runs, skipped, 6, func() report {
			w := wantSeven()
			w.Days, w.SinceNS = 6, reportNow.Add(-6*24*time.Hour).UnixNano()
			w.Runs, w.Attempts = 38, 42
			w.Heaviest[1].Runs, w.Heaviest[1].RanS, w.Heaviest[1].WaitedS = 6, 360, 885.75
			return w
		}()},
		{"one day holds nothing", runs, skipped, 1, empty(1, 2)},
		{"empty log", nil, 0, 7, empty(7, 0)},
		{"a label below the threshold", slices.DeleteFunc(slices.Clone(runs), func(r store.RunRecord) bool {
			return r.Label == "gate:unit" && r.RanS == 10 // one of its alone runs: 9 left
		}), skipped, 7, func() report {
			w := wantSeven()
			w.Runs, w.Attempts = 38, 42
			w.Heaviest[2].Runs, w.Heaviest[2].RanS, w.Heaviest[2].WaitedS = 19, 380, 4.75
			w.Sharing.TooFew, w.Sharing.Labels = 4, []shareLabel{}
			return w
		}()},
		{"a wait of exactly one second counts", append(slices.Clone(runs), store.RunRecord{
			TimeNS: reportNow.Add(-time.Hour).UnixNano(), Label: "lint", WaitedS: 1, RanS: 5}), skipped, 7, func() report {
			w := wantSeven()
			w.Runs, w.Attempts = 40, 44
			w.Waits = reportWaits{Waited: 5, TotalS: 886, MedianS: 30, P90S: 700, LongestS: 700, Over2Min: 2, Over10Min: 1}
			w.Heaviest[3].Runs, w.Heaviest[3].RanS, w.Heaviest[3].WaitedS = 11, 55, 3.5
			return w
		}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildReport(c.runs, c.skipped, reportNow, c.days)
			if !reflect.DeepEqual(got, c.want) {
				g, _ := json.MarshalIndent(got, "", "  ")
				w, _ := json.MarshalIndent(c.want, "", "  ")
				t.Errorf("report:\n%s\nwant:\n%s", g, w)
			}
		})
	}
}

// The text carries each figure, the estimate's caveat and the fixed line on what is not measured,
// and never a saving. Mutation: drop the caveat line under the estimate.
func TestReportText(t *testing.T) {
	runs, skipped := reportFixture(t)
	out := buildReport(runs, skipped, reportNow, 7).human()
	for _, want := range []string{
		"window        last 7 days, since 2026-10-28 12:00 UTC\n",
		"runs          39 in 4 labels, 43 attempts, 1 outside the budget\n",
		"waits         4 attempts waited 1s or more: total 14m45s, median 1m30s, p90 11m40s, longest 11m40s\n",
		"              2 over 2 min, 1 over 10 min\n",
		"stops         5 attempts stopped for memory, 2m00s of work redone, 1 gave up after 3 stops\n",
		"  label      runs  peak     need   ran    waited\n",
		"  e2e        2     3.9 GiB  3809M  3m00s  1s\n",
		"  build      7     2.9 GiB  2930M  7m00s  14m46s\n",
		"estimate      sharing cost of gate:unit: 1.69x, the median run 24s alongside other coreslot runs (10) against 14s alone (10)\n" +
			"              (estimate; caches and load outside coreslot not controlled)\n",
		"not measured  what these runs would have done without coreslot; on a different machine,\n",
		"skipped 2 run-log lines that did not decode\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "saved") {
		t.Errorf("report claims a saving:\n%s", out)
	}
}

func TestReportTextEmpty(t *testing.T) {
	out := buildReport(nil, 0, reportNow, 7).human()
	for _, want := range []string{"runs          none recorded in the window\n",
		"estimate      sharing cost: no label has 10 runs both alone and alongside other coreslot runs\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// --json is the report, whole: decoded, it equals the report built here for the same moment.
// Mutation: tag a field `json:"-"` (days then neither encodes nor decodes).
func TestReportJSONRoundTrip(t *testing.T) {
	dir := reportDir(t)
	r := cli(t, baseEnv(dir), "report", "--json", "--days", "3650")
	if r.code != 0 {
		t.Fatalf("report --json exited %d: %s", r.code, r.stderr)
	}
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	dec.DisallowUnknownFields()
	var got report
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	runs, skipped := reportFixture(t)
	want := buildReport(runs, skipped, time.Unix(0, got.UntilNS), 3650)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("report --json:\n%s\nwant %+v", r.stdout, want)
	}
	if got.Attempts != 45 || got.Schema != 1 {
		t.Errorf("attempts %d, schema %d; want 45, 1", got.Attempts, got.Schema)
	}
}

func TestReportCLI(t *testing.T) {
	if r := cli(t, baseEnv(stateDir(t, "")), "report"); r.code != 0 || !strings.Contains(r.stdout, "none recorded in the window") {
		t.Errorf("report without a log: exit %d, %q, %q", r.code, r.stdout, r.stderr)
	}
	for _, days := range []string{"0", "-1", "36501", "x"} {
		if r := cli(t, baseEnv(stateDir(t, "")), "report", "--days", days); r.code != exitUsage {
			t.Errorf("report --days %s: exit %d, want %d", days, r.code, exitUsage)
		}
	}
	if r := cli(t, baseEnv(stateDir(t, "")), "report", "--help"); r.code != 0 || !strings.HasPrefix(r.stdout, "usage: coreslot report") {
		t.Errorf("report --help: exit %d, %q", r.code, r.stdout)
	}
}
