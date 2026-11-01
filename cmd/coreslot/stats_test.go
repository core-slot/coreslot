//go:build unix

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// statsDir is a state directory whose run log is testdata/runs-stats.jsonl.
func statsDir(t *testing.T) string {
	t.Helper()
	dir := stateDir(t, "")
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

// The text names each label's runs, its window and the need learned from it; a fail-open run's
// line changes none of it. Mutation: learn from Window[0] instead of the window's p90.
func TestStatsText(t *testing.T) {
	r := cli(t, baseEnv(statsDir(t)), "stats")
	if r.code != 0 {
		t.Fatalf("stats exited %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"fastapi:tests  5 runs", "window  5 runs ",
		`need  2.7 GiB (mem = "2765M")`, "current  none"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stats output lacks %q:\n%s", want, r.stdout)
		}
	}
}

// Mutation: leave skipped_lines at 0.
func TestStatsJSON(t *testing.T) {
	r := cli(t, baseEnv(statsDir(t)), "stats", "--json")
	if r.code != 0 {
		t.Fatalf("stats --json exited %d: %s", r.code, r.stderr)
	}
	var doc struct {
		Schema       int `json:"schema"`
		SkippedLines int `json:"skipped_lines"`
		Labels       []struct {
			Label string  `json:"label"`
			Need  *string `json:"need"`
		} `json:"labels"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	if doc.Schema != 1 || doc.SkippedLines != 1 || len(doc.Labels) != 2 {
		t.Fatalf("schema %d, skipped %d, %d labels; want 1, 1, 2:\n%s", doc.Schema, doc.SkippedLines, len(doc.Labels), r.stdout)
	}
	if l := doc.Labels[0]; l.Label != "fastapi:tests" || l.Need == nil || *l.Need != "2765M" {
		t.Errorf("labels[0] = %+v:\n%s", l, r.stdout)
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
