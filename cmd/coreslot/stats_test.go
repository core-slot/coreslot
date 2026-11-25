//go:build unix

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// statsDir is a state directory whose run log is testdata/runs-stats.jsonl.
func statsDir(t *testing.T, fixture string) string {
	t.Helper()
	dir := stateDir(t, "")
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
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
	r := cli(t, baseEnv(statsDir(t, "runs-stats.jsonl")), "stats")
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
	r := cli(t, baseEnv(statsDir(t, "runs-stats.jsonl")), "stats", "--json")
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

// A label whose runs hold less than their peaks shows the need admission uses, and suggests its
// peak-only need as the pin, in the text and in the JSON. Mutation: suggest NeedMiB as the pin →
// mem = "7G", or "need": "7G" in the JSON.
func TestStatsSustained(t *testing.T) {
	dir := statsDir(t, "runs-stats-sustained.jsonl")
	if r := cli(t, baseEnv(dir), "stats"); r.code != 0 || !strings.Contains(r.stdout, `need  7 GiB sustained (mem = "8G")`) {
		t.Errorf("stats: exit %d:\n%s", r.code, r.stdout)
	}
	if r := cli(t, baseEnv(dir), "stats", "--json"); r.code != 0 || !strings.Contains(r.stdout, `"need": "8G"`) {
		t.Errorf("stats --json: exit %d:\n%s", r.code, r.stdout)
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
	dir := statsDir(t, "runs-stats.jsonl")
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

// Each label shows its reservation window: learned from 3 runs that carry one (1.5 times the
// largest, 100 s, at least 120 s), and the default under 3, which says so. Mutation: printing the
// default as learned drops "(default)" from d's line, red.
func TestStatsWindow(t *testing.T) {
	dir := stateDir(t, "")
	if err := os.Mkdir(filepath.Join(dir, "log"), 0o700); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	for i, label := range []string{"w", "w", "w", "d", "d"} {
		fmt.Fprintf(&log, `{"time_ns":%d,"label":%q,"waited_s":0,"ran_s":1,"mem_window_s":100,"exit":0,"fail_open":""}`+"\n", i+1, label)
	}
	if err := os.WriteFile(filepath.Join(dir, "log", "runs.jsonl"), []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	r := cli(t, baseEnv(dir), "stats")
	for _, want := range []string{"w  3 runs\n  window  3 runs 1970-01-01\n  need  nothing measured\n  reserve  follows use over a 150 s window\n",
		"d  2 runs\n  window  2 runs 1970-01-01\n  need  nothing measured\n  reserve  follows use over a 600 s window (default)\n"} {
		if r.code != 0 || !strings.Contains(r.stdout, want) {
			t.Errorf("stats: exit %d, want %q in:\n%s", r.code, want, r.stdout)
		}
	}
}
