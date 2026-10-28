//go:build unix

package main

import (
	"encoding/json"
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

// A per-worker entry gets cpu_per_worker rewritten from the runs that carry workers (3 cores over
// 8 workers: 0.4), never cpu, and keeps its width and every other key.
// Mutation: write cpu over a per-worker entry, or keep its old cpu_per_worker → red.
func TestSuggestWritePerWorker(t *testing.T) {
	for _, tc := range []struct {
		entry string
		cpu   any
	}{
		{"cpu_per_worker = 2\n", nil},
		{"cpu_per_worker = 2\ncpu = \"1..16\"\ngate = \"w\"\n", "1..16"},
	} {
		dir := statsDir(t)
		writeNeeds(t, dir, "[needs.\"worker:tests\"]\n"+tc.entry)
		r := cli(t, baseEnv(dir), "suggest", "--write", "--label", "worker:tests@8")
		if r.code != 0 || !strings.Contains(r.stdout, "cpu_per_worker = 0.4") {
			t.Fatalf("suggest --write exited %d: %s%s", r.code, r.stdout, r.stderr)
		}
		got := readNeeds(t, filepath.Join(dir, "needs.toml"))
		w := got["worker:tests"]
		if w["cpu_per_worker"] != 0.4 || w["cpu"] != tc.cpu || w["mem"] != "1536M" || w["evidence"] == nil {
			t.Errorf("%q: worker:tests entry %v", tc.entry, w)
		}
		if tc.cpu != nil && w["gate"] != "w" {
			t.Errorf("%q: gate dropped: %v", tc.entry, w)
		}
		if len(got) != 1 {
			t.Errorf("--label wrote other entries: %v", got)
		}
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
