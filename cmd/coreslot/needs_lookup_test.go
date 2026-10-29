//go:build unix

package main

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
		map[string]spec.Range{"cpu": {Min: 1, Max: coldMax()}, "mem": {Min: 16, Max: 16}})
}

// A worker suffix finds the base label's entry. Mutation: look up the label as given → red.
func TestNeedsWorkerSuffix(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"x\"]\ncpu = 3\n")
	wantNeed(t, []string{"--label", "x@10", "--", "true"}, lookup, map[string]spec.Range{"cpu": {Min: 3, Max: 3}})
}

// A per-worker entry sizes the request: tokens are c per worker, and the run keeps c and the
// width for {n}. An explicit --need cpu wins and leaves both zero. Mutation: take the width as
// the token count → cpu 8..8, red; set c under --need → c 0.4, red.
func TestNeedsPerWorkerEntrySizesRequest(t *testing.T) {
	saved := machineCPUs
	t.Cleanup(func() { machineCPUs = saved })
	machineCPUs = func() (int, error) { return 64, nil }
	lookup, _ := lookupOf(t, "[needs.\"x\"]\ncpu_per_worker = 0.4\n")
	o := wantNeed(t, []string{"--label", "x@8", "--", "true"}, lookup, map[string]spec.Range{"cpu": {Min: 4, Max: 4}})
	if o.cpuPerWorker != 0.4 || o.workers != (spec.Range{Min: 8, Max: 8}) {
		t.Errorf("cpuPerWorker %v, workers %v; want 0.4 and 8..8", o.cpuPerWorker, o.workers)
	}
	o = wantNeed(t, []string{"--label", "x", "--", "true"}, lookup, map[string]spec.Range{"cpu": {Min: 1, Max: 26}})
	if o.cpuPerWorker != 0.4 || o.workers != (spec.Range{Min: 1, Max: 64}) {
		t.Errorf("no @K: cpuPerWorker %v, workers %v; want 0.4 and 1..64", o.cpuPerWorker, o.workers)
	}
	o = wantNeed(t, []string{"--label", "x@8", "--need", "cpu=3", "--", "true"}, lookup, map[string]spec.Range{"cpu": {Min: 3, Max: 3}})
	if o.cpuPerWorker != 0 || o.workers != (spec.Range{}) {
		t.Errorf("--need cpu: cpuPerWorker %v, workers %v; want zero", o.cpuPerWorker, o.workers)
	}
}

// A per-worker entry without a width, on a machine whose CPU count is unknown, gets the default
// width as defaultNeed does. Mutation: pass the failed count (0) on → workers 1..1, red.
func TestNeedsPerWorkerUnknownCPUCount(t *testing.T) {
	saved := machineCPUs
	t.Cleanup(func() { machineCPUs = saved })
	machineCPUs = func() (int, error) { return 0, errors.New("no count") }
	lookup, _ := lookupOf(t, "[needs.\"x\"]\ncpu_per_worker = 0.5\n")
	o := wantNeed(t, []string{"--label", "x", "--", "true"}, lookup, map[string]spec.Range{"cpu": {Min: 1, Max: 8}})
	if o.workers != (spec.Range{Min: 1, Max: spec.DefaultMax}) {
		t.Errorf("workers %v, want 1..%d", o.workers, spec.DefaultMax)
	}
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

// --default-need fills a resource that neither --need nor the label's entry names, and only that
// one: an entry's cpu wins over it, and so does an explicit --need. Mutation: apply it before the
// entry → cpu 1..16 where the entry says 2..6, red; apply it over --need → cpu 1..16, red; skip
// it → cpu 1..1, red.
func TestDefaultNeedFillsOnlyWhatNothingNames(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"known\"]\ncpu = \"2..6\"\n")
	o := wantNeed(t, []string{"--label", "other", "--default-need", "cpu=1..", "--", "true"}, lookup,
		map[string]spec.Range{"cpu": {Min: 1, Max: 16}})
	if !maps.Equal(o.named, map[string]bool{"cpu": true}) {
		t.Errorf("named = %v, want cpu", o.named)
	}
	o = wantNeed(t, []string{"--label", "known", "--default-need", "cpu=1..", "--", "true"}, lookup,
		map[string]spec.Range{"cpu": {Min: 2, Max: 6}})
	if len(o.named) != 0 {
		t.Errorf("named = %v, want none: the entry supplied cpu", o.named)
	}
	wantNeed(t, []string{"--label", "other", "--need", "cpu=4", "--default-need", "cpu=1..", "--", "true"}, lookup,
		map[string]spec.Range{"cpu": {Min: 4, Max: 4}})
	wantNeed(t, []string{"--default-need", "cpu=1..", "--", "true"}, lookup,
		map[string]spec.Range{"cpu": {Min: 1, Max: 16}})
}

// An entry set aside as invalid (its minimums and gpu's pass a grant's 512 tokens) leaves
// --default-need in force. Mutation: build the bare request from --need alone → no gpu, red.
func TestDefaultNeedSurvivesASetAsideEntry(t *testing.T) {
	lookup, notices := lookupOf(t, "[needs.\"a\"]\ncpu = \"256\"\nmem = \"32G\"\n")
	wantNeed(t, []string{"--label", "a", "--default-need", "cpu=1..", "--default-need", "gpu=1", "--", "true"}, lookup,
		map[string]spec.Range{"cpu": {Min: 1, Max: 16}, "gpu": {Min: 1, Max: 1}})
	if len(*notices) != 1 {
		t.Errorf("notices %q, want the one setting the entry aside", *notices)
	}
}

// A malformed or duplicate --default-need is a usage error, as --need's are. Mutation: ignore its
// parse error → no error, red.
func TestDefaultNeedMalformed(t *testing.T) {
	for _, args := range [][]string{
		{"--default-need", "cpu=..", "--", "true"},
		{"--default-need", "cpu=1", "--default-need", "cpu=2", "--", "true"},
	} {
		var ue *spec.UsageError
		if _, err := parseRun(args, IO{}, nil); !errors.As(err, &ue) {
			t.Errorf("parseRun(%q) = %v, want a usage error", args, err)
		}
	}
}

// The plugin's offered command queues: on a budget of 8, a label with no entry run with
// `--default-need cpu=1..` takes the whole budget, so a second one waits (here, --no-wait
// refuses it), where without the flag both are admitted with one token each. Mutation: drop the
// fallback → the second run is admitted, red.
func TestDefaultNeedQueuesUnknownLabels(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 8\nmem = 0\n")
	env := baseEnv(dir)
	first := cliCmd(t, env, "run", "--label", "go-test", "--default-need", "cpu=1..", "--", "sleep", "60")
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the first run holding all 8 tokens", func() bool {
		for i := range 8 {
			if !locked(t, dir, "cpu", i) {
				return false
			}
		}
		return true
	})
	if r := cli(t, env, "run", "--no-wait", "--label", "go-test", "--default-need", "cpu=1..", "--", "true"); r.code != exitRefusal {
		t.Errorf("second run: exit %d, want %d (refused, the budget is full)\n%s", r.code, exitRefusal, r.stderr)
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

// The mark is the flag's value when given, else the entry's, else unset; TestPreemptibleRequestsOneCPU
// has the other cases. Mutation: let an entry marked false clear the flag's mark → the first row
// red; look the entry up under the label with its @N → the worker count row red.
func TestPreemptibleMark(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"on\"]\npreemptible = true\n[needs.\"off\"]\npreemptible = false\n")
	for _, c := range []struct {
		name string
		args []string
		want bool
	}{
		{"flag beats an entry marked false", []string{"--preemptible", "--label", "off", "--", "true"}, true},
		{"entry true under a worker count", []string{"--label", "on@4", "--", "true"}, true},
	} {
		o, err := parseRun(c.args, IO{}, lookup)
		if err != nil {
			t.Errorf("%s: parseRun(%q): %v", c.name, c.args, err)
			continue
		}
		o.queued = true // the mark applies to a run that queues
		if got := o.marked(); got != c.want {
			t.Errorf("%s: marked() = %v, want %v", c.name, got, c.want)
		}
	}
	o, err := parseRun([]string{"--", "true"}, IO{}, lookup)
	if err != nil || o.preemptible != nil {
		t.Errorf("without a flag or a label, preemptible = %v (%v), want unset", o.preemptible, err)
	}
}

// A preemptible run asks the budget for one cpu token whatever its cpu request, and keeps its
// mem; the unshrunk request stays in need, for the placeholders and the run log. Mutation: shrink
// mem too → every mem check red; skip the shrink for the flag → the flag row red; shrink an
// unmarked run → the last three rows red.
func TestPreemptibleRequestsOneCPU(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"fix\"]\ncpu = \"14\"\nmem = \"2G\"\npreemptible = true\n"+
		"[needs.\"range\"]\ncpu = \"4..14\"\nmem = \"2G\"\npreemptible = true\n"+
		"[needs.\"off\"]\ncpu = \"14\"\nmem = \"2G\"\npreemptible = false\n"+
		"[needs.\"plain\"]\ncpu = \"14\"\nmem = \"2G\"\n")
	memTokens := spec.MemTokensCeil(2048)
	for _, c := range []struct {
		name     string
		args     []string
		cpu, ask spec.Range // the request admission sees, and the one the run keeps
	}{
		{"entry, fixed", []string{"--label", "fix"}, spec.Range{Min: 1, Max: 1}, spec.Range{Min: 14, Max: 14}},
		{"entry, range", []string{"--label", "range"}, spec.Range{Min: 1, Max: 1}, spec.Range{Min: 4, Max: 14}},
		{"flag", []string{"--preemptible", "--label", "plain"}, spec.Range{Min: 1, Max: 1}, spec.Range{Min: 14, Max: 14}},
		{"flag false beats the entry", []string{"--preemptible=false", "--label", "fix"}, spec.Range{Min: 14, Max: 14}, spec.Range{Min: 14, Max: 14}},
		{"entry false", []string{"--label", "off"}, spec.Range{Min: 14, Max: 14}, spec.Range{Min: 14, Max: 14}},
		{"no mark", []string{"--label", "plain"}, spec.Range{Min: 14, Max: 14}, spec.Range{Min: 14, Max: 14}},
	} {
		o, err := parseRun(append(c.args, "--", "true"), IO{}, lookup)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		o.queued = true // the mark applies to a run that queues
		req := o.request(0).Need
		if req["cpu"] != c.cpu || req["mem"] != (spec.Range{Min: memTokens, Max: memTokens}) {
			t.Errorf("%s: the request is %v, want cpu %v and mem %d", c.name, req, c.cpu, memTokens)
		}
		if o.need["cpu"] != c.ask || o.minGrant()["cpu"] != c.cpu.Min {
			t.Errorf("%s: the run keeps cpu %v and minGrant cpu %d, want %v and %d", c.name, o.need["cpu"], o.minGrant()["cpu"], c.ask, c.cpu.Min)
		}
	}
}
