//go:build unix

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/store"
)

// reportNow is the fixture's "now": testdata/runs-report.jsonl places its runs from six days
// before it, one per hour, with a run at exactly seven days before it, one a nanosecond earlier,
// and one an hour after it.
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

func mib(n int64) *int64 { return &n }

// wantSeven is the fixture's report over seven days, every figure worked out by hand from the
// fixture. build waited 0.5, 1, 2, 3, 4, 5, 30, 120, 150, 600, 700 and 900 s and once ran outside
// the budget, and two runs before the window peaked at twice its peak inside it; e2e was stopped
// twice and then finished, and in another run gave up after its third stop. Four lines are not
// plausible (one in the future, a negative run time, a wait above a year, a negative peak) and
// count as skipped with the two that do not decode.
func wantSeven() report {
	return report{
		Schema: 1, Days: 7, SinceNS: reportNow.Add(-7 * 24 * time.Hour).UnixNano(), UntilNS: reportNow.UnixNano(),
		SkippedLines: 6,
		Runs:         29, // build 14, gate:unit 10, lint 3, e2e 2 (one finished, one gave up)
		Attempts:     33,
		FailOpenRuns: 1,
		Labels:       4,
		// The 11 waits of 1 s or more; exactly 120 s and 600 s are not over 2 and 10 min.
		Waits: reportWaits{Attempts: 11, TotalS: 2515, MedianS: 30, P90S: 700, LongestS: 900, Over2Min: 4, Over10Min: 2},
		Stops: reportStops{StoppedAttempts: 5, LostS: 40 + 50 + 10 + 10 + 10, GaveUpRuns: 1},
		Heaviest: []reportLabel{
			{Label: "e2e", Runs: 2, Attempts: 6, PeakMiB: mib(4004), NeedMiB: mib(3809), RanS: 180, WaitedS: 1.5},
			// The need comes from the whole log, the peak from the window.
			{Label: "build", Runs: 14, Attempts: 14, PeakMiB: mib(2930), NeedMiB: mib(5860), RanS: 840, WaitedS: 2516},
			{Label: "gate:unit", Runs: 10, Attempts: 10, PeakMiB: mib(986), NeedMiB: mib(986), RanS: 145, WaitedS: 2.5},
			{Label: "lint", Runs: 3, Attempts: 3, PeakMiB: mib(196), NeedMiB: mib(196), RanS: 15, WaitedS: 0.75},
		},
	}
}

// Every figure of the report, over the fixture and over cuts of it. Mutations: filter the window
// with > instead of >= (the run at exactly seven days drops); count over 2 min with >= 120 (5, not
// 4); take p90 as the longest wait; learn the need from the window instead of the whole log
// (build's need 2930); count a stopped attempt as a run; keep lines from the future (e2e is no
// longer the heaviest).
func TestBuildReport(t *testing.T) {
	runs, skipped := reportFixture(t)
	if skipped != 2 || len(runs) != 40 {
		t.Fatalf("fixture: %d records, %d skipped; want 40, 2", len(runs), skipped)
	}
	empty := func(days int, skipped int) report {
		return report{Schema: 1, Days: days, SinceNS: reportNow.Add(-time.Duration(days) * 24 * time.Hour).UnixNano(),
			UntilNS: reportNow.UnixNano(), SkippedLines: skipped, Heaviest: []reportLabel{}}
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
			w.Runs, w.Attempts = 28, 32
			b := &w.Heaviest[1]
			b.Runs, b.Attempts, b.RanS, b.WaitedS = 13, 13, 780, 2515.75
			return w
		}()},
		{"one day holds nothing", runs, skipped, 1, empty(1, 6)},
		{"empty log", nil, 0, 7, empty(7, 0)},
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

// A label with container work in the window says so, and only says so: its figures are those of
// the same records without the mark. Mutations: set the flag from the whole log → c is marked, red;
// let the flag leave a record out of the peak → the equality fails, red; drop the
// shown-labels-only cut → hidden is named, red.
func TestReportMarksContainerLabel(t *testing.T) {
	at := reportNow.Add(-time.Hour).UnixNano()
	recs := []store.RunRecord{
		{TimeNS: at, Label: "a", RanS: 10, MemPeakKiB: mib(4096), Container: true},
		{TimeNS: at, Label: "a", RanS: 20, MemPeakKiB: mib(2048)},
		{TimeNS: at, Label: "b", RanS: 30, MemPeakKiB: mib(1024)},
		{TimeNS: reportNow.Add(-30 * 24 * time.Hour).UnixNano(), Label: "c", Container: true},
		{TimeNS: at, Label: "c", RanS: 40, MemPeakKiB: mib(512)},
	}
	rep := buildReport(recs, 0, reportNow, 7)
	out := rep.human()
	if want := "container labels: a (peak and need are the client's)\n"; !strings.Contains(out, want) {
		t.Errorf("report lacks %q:\n%s", want, out)
	}
	plain := slices.Clone(recs)
	for i := range plain {
		plain[i].Container = false
	}
	want := buildReport(plain, 0, reportNow, 7)
	if strings.Contains(want.human(), "container labels") {
		t.Errorf("a report with no container label says so:\n%s", want.human())
	}
	got := rep
	got.Heaviest = slices.Clone(rep.Heaviest)
	for i := range got.Heaviest {
		got.Heaviest[i].Container = false
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("figures with the mark:\n%+v\nwithout it:\n%+v", got, want)
	}
	b, err := json.Marshal(rep.Heaviest)
	if err != nil {
		t.Fatal(err)
	}
	var doc []map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, l := range doc {
		if v, ok := l["container"]; (l["label"] == "a") != ok || ok && v != true {
			t.Errorf("label %v: container %v, %v; want true on a alone", l["label"], v, ok)
		}
	}

	recs = recs[:0]
	for i := range heaviestShown + 1 {
		recs = append(recs, store.RunRecord{TimeNS: at, Label: strconv.Itoa(i), MemPeakKiB: mib(int64(9-i) << 10)})
	}
	recs[heaviestShown].Label, recs[heaviestShown].Container = "hidden", true
	if out := buildReport(recs, 0, reportNow, 7).human(); strings.Contains(out, "container labels") {
		t.Errorf("the line names a label the table does not show:\n%s", out)
	}
}

// The text carries each figure and the fixed line on what is not measured, with no result (a
// ratio, a percentage) in it and no saving anywhere. Mutations: print the need in the needs
// file's unit ("5860M"); put "0.40 of unwrapped" back into the line.
func TestReportText(t *testing.T) {
	runs, skipped := reportFixture(t)
	out := buildReport(runs, skipped, reportNow, 7).human()
	for _, want := range []string{
		"window        last 7 days, since 2026-10-28 12:00 UTC\n",
		"runs          29 in 4 labels (33 attempts; 1 run outside the budget)\n",
		"waits         11 attempts waited 1s or more: total 41m55s, median 30s, p90 11m40s, longest 15m00s\n",
		"              4 waited over 2 min, 2 over 10 min\n",
		"stops         5 attempts stopped for memory, 2m00s of run time lost to stops\n",
		"gave up       1 run, after 3 stops\n",
		"  label      runs  attempts  peak     need     ran     waited\n",
		"  e2e        2     6         3.9 GiB  3.7 GiB  3m00s   1s\n",
		"  build      14    14        2.9 GiB  5.7 GiB  14m00s  41m56s\n",
		"not measured  what these runs would have done without coreslot is not measured here;\n" +
			"              results measured on one 16 GB MacBook Air, with and without room, are in\n" +
			"              https://github.com/core-slot/coreslot/blob/main/evidence/air-v04/README.md\n",
		"skipped       6 run-log lines: undecodable, in the future or holding an impossible value\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	notMeasured := out[strings.Index(out, "not measured"):strings.Index(out, "skipped")]
	if strings.Contains(strings.ToLower(out), "saved") || regexp.MustCompile(`[0-9]\.[0-9]|[0-9]x\b|%`).MatchString(notMeasured) {
		t.Errorf("report claims a saving, or a number on what it did not measure:\n%s", out)
	}
}

func TestReportTextEmpty(t *testing.T) {
	out := buildReport(nil, 0, reportNow, 7).human()
	if !strings.Contains(out, "runs          none recorded in the window\n") || strings.Contains(out, "skipped") {
		t.Errorf("report of an empty log:\n%s", out)
	}
}

// The JSON document's keys and shape are pinned: a key renamed, added or removed changes these
// bytes. Mutation: rename fail_open_runs back to fail_open.
func TestReportJSONGolden(t *testing.T) {
	runs, skipped := reportFixture(t)
	f, err := os.CreateTemp(t.TempDir(), "report")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReportJSON(IO{Stdout: f}, buildReport(runs, skipped, reportNow, 7)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "report.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("report --json is not testdata/report.golden.json; a change to the document raises reportSchema:\n%s", got)
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
	if got.Attempts == 0 || got.Schema != 1 {
		t.Errorf("attempts %d, schema %d; want some, 1", got.Attempts, got.Schema)
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
