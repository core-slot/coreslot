//go:build linux

package main

import (
	"os"
	"strings"
	"testing"
)

// The command inherits an OOM score that puts it first in line for the kernel's OOM killer.
// Mutation: leaving the score alone reads 0, which differs only where the inherited score is below 500.
func TestOOMScoreInherited(t *testing.T) {
	dir := stateDir(t, "")
	r := cli(t, baseEnv(dir), "run", "--", "cat", "/proc/self/oom_score_adj")
	if got := strings.TrimSpace(r.stdout); r.code != 0 || got != "500" {
		t.Errorf("the command's oom_score_adj %q (exit %d, stderr %q), want 500", got, r.code, r.stderr)
	}
}

// A label that must not be restarted is not put first in line for the OOM killer either: it keeps the
// score this process has, which a CI runner may already have set. Mutation: raising every run's score.
func TestOOMScoreLeftForNoRestart(t *testing.T) {
	dir := stateDir(t, "")
	writeNeeds(t, dir, "[needs.keep]\nno_restart = true\n")
	own, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil {
		t.Fatal(err)
	}
	r := cli(t, baseEnv(dir), "run", "--label", "keep", "--", "cat", "/proc/self/oom_score_adj")
	if got, want := strings.TrimSpace(r.stdout), strings.TrimSpace(string(own)); r.code != 0 || got != want {
		t.Errorf("the no_restart command's oom_score_adj %q (exit %d, stderr %q), want it left at %q", got, r.code, r.stderr, want)
	}
}
