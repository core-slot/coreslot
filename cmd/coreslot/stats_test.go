//go:build unix

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// statsDir is a state directory of cpu = 10 whose run log is testdata/runs-stats.jsonl.
func statsDir(t *testing.T) string {
	t.Helper()
	dir := stateDir(t, "[resources]\ncpu = 10\n")
	data, err := os.ReadFile(filepath.Join("testdata", "runs-stats.jsonl"))
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

// readNeeds decodes needs.toml's [needs] table.
func readNeeds(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	var doc struct{ Needs map[string]map[string]any }
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Needs
}

// The text names each label's runs, its cores, its peak and the suggestion; a fail-open run's 9
// cores and the worker counts change none of it.
// Mutation: suggest from Window[0] instead of the window's largest cores used.
func TestStatsText(t *testing.T) {
	r := cli(t, baseEnv(statsDir(t)), "stats")
	if r.code != 0 {
		t.Fatalf("stats exited %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"fastapi:tests  5 runs", "max 5.6 cores", "peak 2.7 GiB (footprint) at 22s",
		`suggest  cpu = "1..6"  mem = "2816M"`, "current  none"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stats output lacks %q:\n%s", want, r.stdout)
		}
	}
}

// Mutation: leave skipped_lines at 0, or cores_per_worker null.
func TestStatsJSON(t *testing.T) {
	r := cli(t, baseEnv(statsDir(t)), "stats", "--json")
	if r.code != 0 {
		t.Fatalf("stats --json exited %d: %s", r.code, r.stderr)
	}
	var doc struct {
		Schema       int `json:"schema"`
		SkippedLines int `json:"skipped_lines"`
		Labels       []struct {
			Label      string `json:"label"`
			Suggestion struct {
				CPU            *string  `json:"cpu"`
				Mem            *string  `json:"mem"`
				CoresPerWorker *float64 `json:"cores_per_worker"`
			} `json:"suggestion"`
		} `json:"labels"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	if doc.Schema != 1 || doc.SkippedLines != 1 || len(doc.Labels) != 3 {
		t.Fatalf("schema %d, skipped %d, %d labels; want 1, 1, 3:\n%s", doc.Schema, doc.SkippedLines, len(doc.Labels), r.stdout)
	}
	l := doc.Labels[0]
	if l.Label != "fastapi:tests" || l.Suggestion.Mem == nil || *l.Suggestion.Mem != "2816M" || l.Suggestion.CPU == nil || *l.Suggestion.CPU != "1..6" {
		t.Errorf("labels[0] = %+v:\n%s", l, r.stdout)
	}
	if c := doc.Labels[2].Suggestion.CoresPerWorker; l.Suggestion.CoresPerWorker != nil || c == nil || *c != 0.4 {
		t.Errorf("cores_per_worker %v and %v, want null and 0.4:\n%s", l.Suggestion.CoresPerWorker, c, r.stdout)
	}
}

func TestStatsNoLog(t *testing.T) {
	r := cli(t, baseEnv(stateDir(t, "")), "stats")
	if r.code != 0 || !strings.Contains(r.stdout, "no runs recorded") {
		t.Errorf("stats without a log: exit %d, %q, %q", r.code, r.stdout, r.stderr)
	}
}

// Mutation: write the file without --write.
func TestSuggestDryRunWritesNothing(t *testing.T) {
	dir := statsDir(t)
	r := cli(t, baseEnv(dir), "suggest")
	if r.code != 0 {
		t.Fatalf("suggest exited %d: %s", r.code, r.stderr)
	}
	var doc struct{ Needs map[string]map[string]any }
	if _, err := toml.Decode(r.stdout, &doc); err != nil {
		t.Fatalf("suggest printed no TOML: %v\n%s", err, r.stdout)
	}
	if e := doc.Needs["fastapi:tests"]; e["cpu"] != "1..6" || e["mem"] != "2816M" {
		t.Errorf("printed entry %v:\n%s", e, r.stdout)
	}
	if _, err := os.Lstat(filepath.Join(dir, "needs.toml")); !os.IsNotExist(err) {
		t.Errorf("needs.toml after a dry run: %v", err)
	}
}

// A pinned entry stays as the user wrote it, and keys suggest does not set stay in every entry.
// Mutation: ignore pinned.
func TestSuggestWriteKeepsPinnedAndUnknown(t *testing.T) {
	dir := statsDir(t)
	path := filepath.Join(dir, "needs.toml")
	writeNeeds(t, dir, `[needs."pinned:job"]
cpu = 4
mem = "8G"
pinned = true

[needs."fastapi:tests"]
cpu = "1..16"
gate = "fastapi"
`)
	r := cli(t, baseEnv(dir), "suggest", "--write")
	if r.code != 0 {
		t.Fatalf("suggest --write exited %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "pinned:job: pinned, kept") {
		t.Errorf("output does not name the pinned entry:\n%s", r.stdout)
	}
	got := readNeeds(t, path)
	if p := got["pinned:job"]; p["cpu"] != int64(4) || p["mem"] != "8G" || p["pinned"] != true || p["evidence"] != nil {
		t.Errorf("pinned entry %v", p)
	}
	f := got["fastapi:tests"]
	if f["cpu"] != "1..6" || f["mem"] != "2816M" || f["gate"] != "fastapi" || !strings.HasPrefix(f["evidence"].(string), "5 runs ") {
		t.Errorf("fastapi:tests entry %v", f)
	}
}

// suggestWorkers runs suggest --write over a worker:tests entry, whose one run used 3 cores over
// 8 workers on 3 tokens (saturated, 0.4 per worker), and returns the entry as written.
func suggestWorkers(t *testing.T, entry string) map[string]any {
	t.Helper()
	dir := statsDir(t)
	writeNeeds(t, dir, "[needs.\"worker:tests\"]\n"+entry)
	r := cli(t, baseEnv(dir), "suggest", "--write", "--label", "worker:tests@8")
	got := readNeeds(t, filepath.Join(dir, "needs.toml"))
	if r.code != 0 || !strings.Contains(r.stdout, "cpu_per_worker = 0.4") || len(got) != 1 {
		t.Fatalf("suggest --write exited %d, wrote %v: %s%s", r.code, got, r.stdout, r.stderr)
	}
	return got["worker:tests"]
}

// A per-worker entry gets cpu_per_worker rewritten, never cpu, and keeps every other key; the
// evidence counts the saturated run. Mutation: write cpu over a per-worker entry, keep its old
// cpu_per_worker, or drop the saturated count from the evidence → red.
func TestSuggestWritePerWorker(t *testing.T) {
	if w := suggestWorkers(t, "cpu_per_worker = 2\ngate = \"w\"\n"); w["cpu_per_worker"] != 0.4 || w["cpu"] != nil || w["gate"] != "w" || w["mem"] != "1536M" ||
		!strings.Contains(fmt.Sprint(w["evidence"]), "1 of 1 saturated") {
		t.Errorf("worker:tests entry %v", w)
	}
}

// Mutation: exit 70 on the needs file's error.
func TestSuggestWriteMalformed(t *testing.T) {
	dir := statsDir(t)
	const body = "[needs\nnot toml\n"
	path := filepath.Join(dir, "needs.toml")
	writeNeeds(t, dir, body)
	r := cli(t, baseEnv(dir), "suggest", "--write")
	if r.code != exitProblems || !strings.Contains(r.stderr, "needs.toml") {
		t.Errorf("suggest --write over a malformed file: exit %d, %q; want %d naming it", r.code, r.stderr, exitProblems)
	}
	if data, _ := os.ReadFile(path); string(data) != body {
		t.Errorf("the malformed file changed: %q", data)
	}
}

// A needs file that cannot be read is one notice for stats and a dry-run suggest, which go on as
// if it had no entries. Mutation: fail learn on the needs file's error → exit 1, red.
func TestStatsMalformedNeedsIsANotice(t *testing.T) {
	dir := statsDir(t)
	writeNeeds(t, dir, "[needs\nnot toml\n")
	for _, args := range [][]string{{"stats"}, {"stats", "--json"}, {"suggest"}} {
		r := cli(t, baseEnv(dir), args...)
		if r.code != 0 || strings.Count(r.stderr, "needs.toml") != 1 || !strings.HasSuffix(r.stderr, "; ignoring it\n") {
			t.Errorf("%q over a malformed needs file: exit %d, stderr %q; want 0 and one notice", args, r.code, r.stderr)
		}
	}
	if r := cli(t, baseEnv(dir), "stats"); !strings.Contains(r.stdout, "current  none") {
		t.Errorf("stats over a malformed needs file:\n%s", r.stdout)
	}
}

// unobservedDir is a state directory whose log has three 120 s runs of ci:box at 0.001 cores and
// three of ci:busy at 0.5, each with a 1 GiB peak.
func unobservedDir(t *testing.T, entries string) string {
	t.Helper()
	dir, log := statsDir(t), ""
	writeNeeds(t, dir, entries)
	for _, c := range []struct {
		label string
		cpu   float64
	}{{"ci:box", 0.12}, {"ci:busy", 60}} {
		log += strings.Repeat(fmt.Sprintf(`{"time_ns":1792590000000000000,"label":%q,"need":{"cpu":[2,2]},"grant":{"cpu":2},"ran_s":120,"cpu_user_s":%g,"cpu_sys_s":0,"mem_peak_kib":1048576,"fail_open":""}`+"\n", c.label, c.cpu), 3)
	}
	f, err := os.OpenFile(filepath.Join(dir, "log", "runs.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err == nil {
		_, err = f.WriteString(log)
	}
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// An entry with observed = false is kept as written: no cpu or mem from its near-zero cores.
// Mutation: ignore observed → the entry gets cpu = "1" and evidence.
func TestSuggestKeepsUnobservedEntry(t *testing.T) {
	dir := unobservedDir(t, "[needs.\"ci:box\"]\ncpu = 2\nmem = \"4G\"\nobserved = false\n")
	r := cli(t, baseEnv(dir), "suggest", "--write", "--label", "ci:box")
	if e := readNeeds(t, filepath.Join(dir, "needs.toml"))["ci:box"]; r.code != 0 || e["cpu"] != int64(2) || e["mem"] != "4G" || e["evidence"] != nil ||
		!strings.Contains(r.stdout, "observed = false: its work runs outside the process tree, kept") {
		t.Errorf("exit %d, entry %v:\n%s%s", r.code, e, r.stdout, r.stderr)
	}
}

// Runs of 120 s under 0.05 cores lower nothing and carry the hint; 0.5 cores is learned, no hint.
// Mutation: drop the hint, or lower cpu anyway → red.
func TestSuggestHintsUnobservedWork(t *testing.T) {
	dir := unobservedDir(t, "[needs.\"ci:box\"]\ncpu = 2\nmem = \"4G\"\n")
	r := cli(t, baseEnv(dir), "suggest", "--write", "--label", "ci:box")
	r2 := cli(t, baseEnv(dir), "suggest", "--write", "--label", "ci:busy")
	got := readNeeds(t, filepath.Join(dir, "needs.toml"))
	box, busy := got["ci:box"], got["ci:busy"]
	if r.code != 0 || r2.code != 0 || box["cpu"] != int64(2) || box["mem"] != "4G" || !strings.Contains(fmt.Sprint(box["evidence"]), "set observed = false or pin") ||
		busy["cpu"] != "1" || strings.Contains(fmt.Sprint(busy["evidence"]), "observed") {
		t.Errorf("ci:box %v, ci:busy %v:\n%s%s", box, busy, r.stderr, r2.stderr)
	}
}
