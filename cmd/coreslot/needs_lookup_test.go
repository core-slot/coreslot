//go:build unix

package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
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

func wantNeed(t *testing.T, args []string, lookup needsLookup, want map[string]spec.Range) {
	t.Helper()
	o, err := parseRun(args, IO{}, lookup)
	if err != nil {
		t.Fatalf("parseRun(%q): %v", args, err)
	}
	if !maps.Equal(o.need, want) {
		t.Errorf("parseRun(%q) need = %v, want %v", args, o.need, want)
	}
}

// The entry's mem is the request when --mem is not given, and --mem wins over it. Mutation: let
// the entry override --mem → 16 tokens, red; ignore the entry → one token, red.
func TestNeedsEntryMem(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"a\"]\nmem = \"2G\"\n")
	wantNeed(t, []string{"--label", "a", "--", "true"}, lookup, map[string]spec.Range{"mem": {Min: 16, Max: 16}})
	wantNeed(t, []string{"--label", "a", "--mem", "512M", "--", "true"}, lookup, map[string]spec.Range{"mem": {Min: 4, Max: 4}})
	wantNeed(t, []string{"--label", "other", "--", "true"}, lookup, map[string]spec.Range{"mem": {Min: 1, Max: 1}})
}

// An unlabelled run never reads the needs file. Mutation: drop the label check → red.
func TestNeedsUnlabelled(t *testing.T) {
	calls := 0
	lookup := func(string) (needs.Entry, bool) { calls++; return needs.Entry{}, false }
	wantNeed(t, []string{"--", "true"}, lookup, map[string]spec.Range{"mem": {Min: 1, Max: 1}})
	if calls != 0 {
		t.Errorf("lookup called %d times for an unlabelled run", calls)
	}
}

// writeNeeds writes the needs file of the state directory dir (its configuration's sibling).
func writeNeeds(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "needs.toml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A pass-through says nothing about a mem the needs file supplied. Mutation: announce every
// clamp → red.
func TestNestedQuietOnEntryMem(t *testing.T) {
	dir := stateDir(t, "[resources]\nmem = \"1G\"\n")
	writeNeeds(t, dir, "[needs.\"inner\"]\nmem = \"2G\"\n")
	r := outer(t, dir, "256M", `"$BIN" run --label inner -- sh -c 'echo $CORESLOT_GRANTED_MEM'`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if got := strings.TrimSpace(r.stdout); got != "256" {
		t.Errorf("the command saw mem %q, want the parent's 256 MiB", got)
	}
	if strings.Contains(r.stderr, "parent run holds") {
		t.Errorf("stderr %q has a notice for a mem the user never typed", r.stderr)
	}
}

// A malformed needs file is one notice and no entry, however often it is looked up. Mutation:
// load on every call → two notices, red.
func TestNeedsLookupUnreadable(t *testing.T) {
	lookup, notices := lookupOf(t, "[needs\n")
	for range 2 {
		if _, ok := lookup("a"); ok {
			t.Error("a malformed file gave an entry")
		}
	}
	if len(*notices) != 1 || !strings.HasPrefix((*notices)[0], "needs file ") || !strings.HasSuffix((*notices)[0], "; ignoring it") {
		t.Errorf("notices %q, want one \"needs file …: …; ignoring it\"", *notices)
	}
}
