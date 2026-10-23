//go:build unix

package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/needs"
	"github.com/core-slot/coreslot/internal/spec"
)

// lookupOf is newNeedsLookup over a needs file holding text, with the notices it prints.
func lookupOf(t *testing.T, text string) (needsLookup, *[]string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "needs.toml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	var notices []string
	return newNeedsLookup(filepath.Join(dir, "config.toml"), func(format string, args ...any) {
		notices = append(notices, fmt.Sprintf(format, args...))
	}), &notices
}

func wantNeed(t *testing.T, args []string, lookup needsLookup, want map[string]spec.Range) runOptions {
	t.Helper()
	o, err := parseRun(args, IO{}, lookup)
	if err != nil {
		t.Fatalf("parseRun(%q): %v", args, err)
	}
	if !maps.Equal(o.need, want) {
		t.Errorf("parseRun(%q) need = %v, want %v", args, o.need, want)
	}
	return o
}

// An explicit --need wins for its resource; the entry fills the rest. Mutation: let the entry
// override an explicit --need cpu → cpu 1..7, red.
func TestNeedsEntryFillsUnnamed(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"a\"]\ncpu = \"1..7\"\nmem = \"3G\"\n")
	o := wantNeed(t, []string{"--label", "a", "--need", "cpu=4", "--", "true"}, lookup,
		map[string]spec.Range{"cpu": {Min: 4, Max: 4}, "mem": {Min: 24, Max: 24}})
	if !maps.Equal(o.named, map[string]bool{"cpu": true}) {
		t.Errorf("named = %v, want cpu only", o.named)
	}
}

// Without --need, the entry's cpu replaces the default. Mutation: apply defaultNeed before the
// entry → cpu 1..1, red.
func TestNeedsEntryReplacesDefaultCPU(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"a\"]\ncpu = \"2..6\"\n")
	o := wantNeed(t, []string{"--label", "a", "--", "true"}, lookup, map[string]spec.Range{"cpu": {Min: 2, Max: 6}})
	if len(o.named) != 0 {
		t.Errorf("named = %v, want none", o.named)
	}
}

// An entry with mem only keeps the default cpu. Mutation: skip the default when the entry
// supplied anything → no cpu, red.
func TestNeedsEntryMemOnlyKeepsDefault(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"a\"]\nmem = \"2G\"\n")
	wantNeed(t, []string{"--label", "a", "--", "echo", "{n}"}, lookup,
		map[string]spec.Range{"cpu": {Min: 1, Max: 16}, "mem": {Min: 16, Max: 16}})
}

// A worker suffix finds the base label's entry. Mutation: look up the label as given → red.
func TestNeedsWorkerSuffix(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"x\"]\ncpu = 3\n")
	wantNeed(t, []string{"--label", "x@10", "--", "true"}, lookup, map[string]spec.Range{"cpu": {Min: 3, Max: 3}})
}

// An unlabelled run never reads the needs file. Mutation: drop the label check → red.
func TestNeedsUnlabelled(t *testing.T) {
	calls := 0
	lookup := func(string) (needs.Entry, bool, func(error)) { calls++; return needs.Entry{}, false, nil }
	wantNeed(t, []string{"--", "true"}, lookup, map[string]spec.Range{"cpu": {Min: 1, Max: 1}})
	if calls != 0 {
		t.Errorf("lookup called %d times for an unlabelled run", calls)
	}
}

// Placeholders are checked after the merge. Mutation: check them before the entry is applied →
// a usage error, red.
func TestPlaceholderMemFromEntry(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"a\"]\nmem = \"2G\"\n")
	wantNeed(t, []string{"--label", "a", "--", "echo", "{n:mem}"}, lookup,
		map[string]spec.Range{"cpu": {Min: 1, Max: 1}, "mem": {Min: 16, Max: 16}})
}

// An entry that makes a valid request invalid is set aside with one notice naming the file, the
// label and the error, and the run goes on without it. Mutation: return the merged request's
// validation error → a usage error, red.
func TestNeedsEntryNeverRefuses(t *testing.T) {
	lookup, notices := lookupOf(t, "[needs.\"a\"]\ncpu = \"256\"\nmem = \"32G\"\n")
	wantNeed(t, []string{"--label", "a", "--need", "gpu=1", "--", "true"}, lookup,
		map[string]spec.Range{"gpu": {Min: 1, Max: 1}})
	if len(*notices) != 1 || !strings.HasPrefix((*notices)[0], "needs file ") ||
		!strings.Contains((*notices)[0], "the entry for a makes the request invalid: ") ||
		!strings.HasSuffix((*notices)[0], "; running without it") {
		t.Errorf("notices %q, want one naming the file, the label and the error", *notices)
	}
}

// writeNeeds writes the needs file of the state directory dir (its configuration's sibling).
func writeNeeds(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "needs.toml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// With mem off, a mem the needs file supplied prints no "not declared" notice, and is still
// recorded as unbudgeted. Mutation: print the notice for every unbudgeted resource → red.
func TestNoNoticeForUnnamedResource(t *testing.T) {
	dir := stateDir(t, "[resources]\nmem = 0\n")
	writeNeeds(t, dir, "[needs.\"lbl\"]\nmem = \"2G\"\n")
	r := cli(t, baseEnv(dir), "run", "--label", "lbl", "--", "true")
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if strings.Contains(r.stderr, "not declared") {
		t.Errorf("stderr %q has a notice for a mem the user never typed", r.stderr)
	}
	lines := runLog(t, dir)
	if len(lines) != 1 || !slices.Contains(anyStrings(lines[0]["unbudgeted_resources"]), "mem") {
		t.Errorf("run log %v, want one line with mem unbudgeted", lines)
	}
}

// A mem --need names still gets the notice. Mutation: drop every unbudgeted notice → red.
func TestNoticeForNamedResource(t *testing.T) {
	dir := stateDir(t, "[resources]\nmem = 0\n")
	r := cli(t, baseEnv(dir), "run", "--need", "mem=2G", "--", "true")
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if want := "coreslot: mem: not declared in the configuration; running unbudgeted with mem=2 GiB"; !strings.Contains(r.stderr, want) {
		t.Errorf("stderr %q, want %q", r.stderr, want)
	}
}

// A pass-through under a parent holding cpu only says nothing about the needs file's mem.
// Mutation: print "holds none" for every unbudgeted resource → red.
func TestNestedQuietOnEntryResource(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 8\n")
	writeNeeds(t, dir, "[needs.\"inner\"]\nmem = \"2G\"\n")
	r := outer(t, dir, []string{"cpu=2"}, `"$BIN" run --label inner -- sh -c 'echo $CORESLOT_GRANTED_MEM'`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if got := strings.TrimSpace(r.stdout); got != "2048" {
		t.Errorf("the command saw mem %q, want 2048 (MiB) from the needs file", got)
	}
	if strings.Contains(r.stderr, "holds none") {
		t.Errorf("stderr %q has a notice for a mem the user never typed", r.stderr)
	}
}

// A malformed needs file is one notice and no entry, however often it is looked up. Mutation:
// load on every call → two notices, red.
func TestNeedsLookupUnreadable(t *testing.T) {
	lookup, notices := lookupOf(t, "[needs\n")
	for range 2 {
		if _, ok, _ := lookup("a"); ok {
			t.Error("a malformed file gave an entry")
		}
	}
	if len(*notices) != 1 || !strings.HasPrefix((*notices)[0], "needs file ") || !strings.HasSuffix((*notices)[0], "; ignoring it") {
		t.Errorf("notices %q, want one \"needs file …: …; ignoring it\"", *notices)
	}
}
