//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
)

// longRuns is n counted runs of label that ran for minutes and used 5 cores.
func longRuns(label string, minutes float64, n int) []store.RunRecord {
	var runs []store.RunRecord
	for i := range n {
		runs = append(runs, store.RunRecord{TimeNS: int64(i + 1), Label: label, RanS: minutes * 60,
			CPUUserS: new(5 * minutes * 60), CPUSysS: new(0.0)})
	}
	return runs
}

// The automatic rule, for a cpu size of 14: a run that nothing marked and that queues is
// preemptible when its request's max is at least half the size and its label has 5 or more
// counted runs with a median of 10 minutes or more that do not look unobserved. A small request
// never opens the run log. Mutations: test the history before the size → the "max 6" row opens the
// log, red; count 4 runs → the 4-run row red; compare the median with 540 s → the 9-minute row
// red; take the mean or the minimum for the median → the skewed row red; drop the unobserved test, the terminal test or the mark test → their rows red; count
// pass-through runs → the "pass-through" row red.
func TestAutoPreemptible(t *testing.T) {
	long := longRuns("x", 14, 5)
	unobserved := longRuns("x", 14, 5)
	for i := range unobserved {
		unobserved[i].CPUUserS = new(10.0)
	}
	nested := append(longRuns("x", 14, 3), longRuns("x", 14, 2)...)
	for _, i := range []int{3, 4} {
		nested[i].Nested = true
	}
	// Two short runs among three of 15 minutes: the median is 15 minutes, the mean is under 10.
	skewed := longRuns("x", 15, 5)
	skewed[0].RanS, skewed[1].RanS = 60, 60
	yes, no := true, false
	for _, c := range []struct {
		name     string
		cpu      spec.Range
		label    string
		mark     *bool
		terminal bool
		runs     []store.RunRecord
		readErr  error
		wantOpen bool // the run log is read
		want     string
	}{
		{"a learned range", spec.Range{Min: 1, Max: 12}, "x", nil, false, long, nil, true, "auto: median 14m >= 10m, cpu 12 >= 7"},
		{"exactly half", spec.Range{Min: 1, Max: 7}, "x", nil, false, long, nil, true, "auto: median 14m >= 10m, cpu 7 >= 7"},
		{"a worker suffix names the same label", spec.Range{Min: 1, Max: 12}, "x@8", nil, false, longRuns("x@4", 14, 5), nil, true, "auto: median 14m >= 10m, cpu 12 >= 7"},
		{"the median, not the mean or the minimum", spec.Range{Min: 1, Max: 12}, "x", nil, false, skewed, nil, true, "auto: median 15m >= 10m, cpu 12 >= 7"},
		{"4 runs", spec.Range{Min: 1, Max: 12}, "x", nil, false, long[:4], nil, true, ""},
		{"median 9 minutes", spec.Range{Min: 1, Max: 12}, "x", nil, false, longRuns("x", 9, 5), nil, true, ""},
		{"max 6", spec.Range{Min: 1, Max: 6}, "x", nil, false, long, nil, false, ""},
		{"another label", spec.Range{Min: 1, Max: 12}, "y", nil, false, long, nil, true, ""},
		{"pass-through runs", spec.Range{Min: 1, Max: 12}, "x", nil, false, nested, nil, true, ""},
		{"preemptible = false", spec.Range{Min: 1, Max: 12}, "x", &no, false, long, nil, false, ""},
		{"already marked", spec.Range{Min: 1, Max: 12}, "x", &yes, false, long, nil, false, ""},
		{"unobserved work", spec.Range{Min: 1, Max: 12}, "x", nil, false, unobserved, nil, true, ""},
		{"terminal mode", spec.Range{Min: 1, Max: 12}, "x", nil, true, long, nil, false, ""},
		{"no label", spec.Range{Min: 1, Max: 12}, "", nil, false, long, nil, false, ""},
		{"an unreadable run log", spec.Range{Min: 1, Max: 12}, "x", nil, false, nil, errors.New("denied"), true, ""},
	} {
		opened := false
		read := func() ([]store.RunRecord, error) { opened = true; return c.runs, c.readErr }
		o := runOptions{label: c.label, preemptible: c.mark, need: map[string]spec.Range{"cpu": c.cpu}}
		got, _ := autoPreemptible(o, c.terminal, 14, read)
		if got != c.want || opened != c.wantOpen {
			t.Errorf("%s: reason %q, run log read %v; want %q and %v", c.name, got, opened, c.want, c.wantOpen)
		}
	}
	// A request that names no cpu has nothing to shrink.
	if got, _ := autoPreemptible(runOptions{label: "x", need: map[string]spec.Range{"mem": {Min: 4, Max: 4}}}, false, 14,
		func() ([]store.RunRecord, error) { t.Error("the run log was read"); return long, nil }); got != "" {
		t.Errorf("a request without cpu: reason %q, want none", got)
	}
}

// The hint about deadlines comes with the first demotion of a label and not after, whichever
// reason the earlier demotion had: an earlier entry's mark counts as an auto one does
// (TestAutoPreemptibleEndToEnd has the first demotion and the auto one). Mutation: look only for
// an auto reason → red.
func TestAutoPreemptibleHintOnce(t *testing.T) {
	o := runOptions{label: "x", need: map[string]spec.Range{"cpu": {Min: 1, Max: 12}}}
	runs := longRuns("x", 14, 5)
	runs[2].Preemptible = "entry"
	if reason, first := autoPreemptible(o, false, 14, func() ([]store.RunRecord, error) { return runs, nil }); reason == "" || first {
		t.Errorf("after an earlier entry: reason %q, first %v; want a reason and not the first", reason, first)
	}
}

// writeRunLog writes lines to a generation of the run log in dir.
func writeRunLog(t *testing.T, dir, name string, runs []store.RunRecord) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, store.LogDir), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range runs {
		fmt.Fprintf(&b, `{"time_ns":%d,"label":%q,"need":{"cpu":[1,12]},"grant":{"cpu":12},"ran_s":%g,"cpu_user_s":%g,"cpu_sys_s":0,"fail_open":""}`+"\n",
			r.TimeNS, r.Label, r.RanS, *r.CPUUserS)
	}
	if err := os.WriteFile(filepath.Join(dir, store.LogDir, name), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A run whose label has the history is demoted by `coreslot run`: the history is read across both
// generations of the run log, the request is one cpu token, the record says why, the first
// demotion of the label prints the deadline hint and the next one does not. Mutation: read only
// runs.jsonl → the run is not demoted, red; print the hint every time → the second run's stderr
// has it, red.
func TestAutoPreemptibleEndToEnd(t *testing.T) {
	const hint = "Runs with wall-clock deadlines (test timeouts, latency assertions, browser or e2e tests, dev-server compile windows, agent tool timeouts) can fail when demoted; set `preemptible = false` for them."
	dir := stateDir(t, "[resources]\ncpu = 14\n")
	writeNeeds(t, dir, "[needs.\"long\"]\ncpu = \"1..12\"\n")
	runs := longRuns("long", 14, 5)
	writeRunLog(t, dir, store.RunLogOld, runs[:2])
	writeRunLog(t, dir, store.RunLog, runs[2:])
	for n, wantHint := range []bool{true, false} {
		r := cli(t, baseEnv(dir), "run", "--label", "long", "--", "true")
		if r.code != 0 || strings.Contains(r.stderr, hint) != wantHint {
			t.Fatalf("run %d: exit %d, stderr %q; want the hint: %v", n, r.code, r.stderr, wantHint)
		}
	}
	lines := runLog(t, dir) // runs.jsonl only: the 3 it had and the 2 new
	if len(lines) != 5 {
		t.Fatalf("%d log lines, want 5", len(lines))
	}
	for _, line := range lines[3:] {
		if line["preemptible"] != "auto: median 14m >= 10m, cpu 12 >= 7" || line["grant"].(map[string]any)["cpu"] != float64(1) ||
			fmt.Sprint(line["need"].(map[string]any)["cpu"]) != "[1 12]" {
			t.Errorf("record %v, want the auto reason, a grant of 1 and the need 1..12", line)
		}
	}
}

// status shows a preemptible hold's reason, in the text and in --json, and leaves it out for any
// other hold. Mutation: drop the key from the hold body → red.
func TestStatusShowsPreemptible(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	cmd := cliCmd(t, baseEnv(dir), "run", "--preemptible", "--label", "batch", "--need", "cpu=1..4", "--", "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	other := cliCmd(t, baseEnv(dir), "run", "--label", "plain", "--", "sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	var text string
	waitFor(t, 30*time.Second, "both holds in status", func() bool {
		text = cli(t, baseEnv(dir), "status").stdout
		return strings.Contains(text, "  batch  pid") && strings.Contains(text, "  plain  pid")
	})
	for _, line := range strings.Split(text, "\n") {
		has := strings.Contains(line, "preemptible (flag)")
		if has != strings.HasPrefix(line, "  batch  pid") {
			t.Errorf("status line %q: preemptible shown %v", line, has)
		}
	}
	js := cli(t, baseEnv(dir), "status", "--json").stdout
	if strings.Count(js, `"preemptible": "flag"`) != 1 || strings.Count(js, `"preemptible"`) != 1 {
		t.Errorf("status --json:\n%s\nwant one preemptible key, \"flag\"", js)
	}
}
