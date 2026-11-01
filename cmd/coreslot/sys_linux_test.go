//go:build linux

package main

import (
	"strings"
	"testing"
)

// The command inherits an OOM score that puts it first in line for the kernel's OOM killer.
// Mutation: leaving the score alone reads 0.
func TestOOMScoreInherited(t *testing.T) {
	dir := stateDir(t, "")
	r := cli(t, baseEnv(dir), "run", "--", "cat", "/proc/self/oom_score_adj")
	if got := strings.TrimSpace(r.stdout); r.code != 0 || got != "500" {
		t.Errorf("the command's oom_score_adj %q (exit %d, stderr %q), want 500", got, r.code, r.stderr)
	}
}
