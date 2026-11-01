//go:build unix

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// A needs file that cannot be read is one notice for stats, which goes on as if it had no
// entries. Mutation: fail learn on the needs file's error → exit 1, red.
func TestStatsMalformedNeedsIsANotice(t *testing.T) {
	dir := statsDir(t)
	writeNeeds(t, dir, "[needs\nnot toml\n")
	for _, args := range [][]string{{"stats"}, {"stats", "--json"}} {
		r := cli(t, baseEnv(dir), args...)
		if r.code != 0 || strings.Count(r.stderr, "needs.toml") != 1 || !strings.HasSuffix(r.stderr, "; ignoring it\n") {
			t.Errorf("%q over a malformed needs file: exit %d, stderr %q; want 0 and one notice", args, r.code, r.stderr)
		}
	}
	if r := cli(t, baseEnv(dir), "stats"); !strings.Contains(r.stdout, "current  none") {
		t.Errorf("stats over a malformed needs file:\n%s", r.stdout)
	}
}
