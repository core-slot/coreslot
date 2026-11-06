//go:build unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/needs"
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

// noLearned is a run log with nothing to say.
func noLearned() (int64, bool) { return 0, false }

// wantNeed parses args as a run on a machine of totalMiB and checks the need it resolves to; learned
// is what the run log says.
func wantNeed(t *testing.T, args []string, lookup needsLookup, learned func() (int64, bool), totalMiB, want int64) {
	t.Helper()
	o, err := parseRun(args, IO{})
	if err != nil {
		t.Fatalf("parseRun(%q): %v", args, err)
	}
	o.resolveNeed(lookup, learned, totalMiB)
	if o.need != want {
		t.Errorf("%q: need = %d MiB, want %d", args, o.need, want)
	}
}

// --mem beats the needs file's pin, which beats what the run log learned, which beats the cold
// start: the smaller of 4 GiB and a quarter of the machine. A label with a space and an "@" is a
// quoted key of the needs file. Mutations: let the pin override --mem → 2048, red; let the learned
// need override the pin → 1536, red; a cold start of total/4 alone → 8192, red.
func TestNeedPrecedence(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"a\"]\nmem = \"2G\"\n[needs.\"go test@proj\"]\nmem = \"3G\"\n")
	learned := func() (int64, bool) { return 1536, true }
	const total = 32768
	wantNeed(t, []string{"--label", "a", "--mem", "512M", "--", "true"}, lookup, learned, total, 512)
	wantNeed(t, []string{"--label", "a", "--", "true"}, lookup, learned, total, 2048)
	wantNeed(t, []string{"--label", "go test@proj", "--", "true"}, lookup, learned, total, 3072)
	wantNeed(t, []string{"--label", "other", "--", "true"}, lookup, learned, total, 1536)
	wantNeed(t, []string{"--label", "other", "--", "true"}, lookup, noLearned, 32768, 4096)
	wantNeed(t, []string{"--label", "other", "--", "true"}, lookup, noLearned, 16384, 4096)
	wantNeed(t, []string{"--label", "other", "--", "true"}, lookup, noLearned, 8192, 2048)
}

// The need's order for a command named by the program rule: --mem, the pin, the learned need, 1 MiB,
// and only for other commands the cold start. A pin is what makes a run pinned: a learned need or
// the program rule's 1 MiB is not. Mutations: put the program rule above the learned need → 1 for
// learned 31, red; drop the program rule → 4096 for a docker with no history, red; apply it to every
// command → 1 for make, red.
func TestNeedOrder(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"pin\"]\nmem = \"2G\"\n")
	learned := func() (int64, bool) { return 31, true }
	const total = 48 << 10
	for _, c := range []struct {
		name    string
		args    []string
		learned func() (int64, bool)
		want    int64
		pinned  bool
	}{
		{"--mem beats all", []string{"--label", "pin", "--mem", "512M", "--", "docker", "run"}, learned, 512, true},
		{"the pin beats learned", []string{"--label", "pin", "--", "docker", "run"}, learned, 2048, true},
		{"learned beats the program rule", []string{"--", "docker", "run"}, learned, 31, false},
		{"the program rule", []string{"--", "docker", "run"}, noLearned, 1, false},
		{"the program rule on a path", []string{"--", "/usr/local/bin/podman", "run"}, noLearned, 1, false},
		{"the cold start", []string{"--", "make", "test"}, noLearned, 4096, false},
	} {
		o, err := parseRun(c.args, IO{})
		if err != nil {
			t.Fatalf("parseRun(%q): %v", c.args, err)
		}
		o.resolveNeed(lookup, c.learned, total)
		if o.need != c.want || o.pinned != c.pinned {
			t.Errorf("%s: need %d MiB, pinned %v; want %d, %v", c.name, o.need, o.pinned, c.want, c.pinned)
		}
	}
}

// A command named by the program rule is no_restart however its need was settled: stopping the
// client would leave its container running and start a second one, so --restart is neither required
// nor honoured. Mutation: set noRestart after the --mem return → the --mem row, red.
func TestContainerCommandIsNoRestart(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"pin\"]\nmem = \"2G\"\n")
	for name, flags := range map[string][]string{
		"no pin":         nil,
		"--mem":          {"--mem", "512M"},
		"needs.toml pin": {"--label", "pin"},
	} {
		for _, restart := range []string{"", "--restart=yes"} {
			args := append(slices.Clone(flags), "--", "docker", "run")
			if restart != "" {
				args = append([]string{restart}, args...)
			}
			o, err := parseRun(args, IO{})
			if err != nil {
				t.Fatalf("parseRun(%q): %v", args, err)
			}
			o.resolveNeed(lookup, noLearned, 32768)
			if !o.noRestart || o.restartable() || o.restartMissing(false) {
				t.Errorf("%s %q: noRestart %v, restartable %v, restartMissing %v; want true, false, false",
					name, restart, o.noRestart, o.restartable(), o.restartMissing(false))
			}
		}
	}
}

// The request carries the mark only for a program-rule command with no pin: a pin restores ordinary
// admission. Mutation: leave out !o.pinned → the --mem and pin rows, red; drop containerCommand →
// the make row, red.
func TestRequestContainerMark(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"pin\"]\nmem = \"2G\"\n")
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"--", "docker", "run"}, true},
		{[]string{"--mem", "512M", "--", "docker", "run"}, false},
		{[]string{"--label", "pin", "--", "docker", "run"}, false},
		{[]string{"--", "make", "test"}, false},
	} {
		o, err := parseRun(c.args, IO{})
		if err != nil {
			t.Fatalf("parseRun(%q): %v", c.args, err)
		}
		o.resolveNeed(lookup, noLearned, 32768)
		if got := o.request().RealMemory; got != c.want {
			t.Errorf("%q: RealMemory %v, want %v", c.args, got, c.want)
		}
	}
}

// A run with no label never reads the needs file. Mutation: drop the label check → red.
func TestNeedsUnlabelled(t *testing.T) {
	calls := 0
	lookup := func(string) (needs.Entry, bool) { calls++; return needs.Entry{}, false }
	wantNeed(t, []string{"--", "true"}, lookup, noLearned, 8192, 2048)
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
	dir := budgetDir(t, 1024)
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
