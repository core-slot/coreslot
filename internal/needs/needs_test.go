//go:build unix

package needs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/core-slot/coreslot/internal/spec"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, path string) File {
	t.Helper()
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestBaseLabel: only a final "@digits" or "@-" is a worker count. Mutation: strip at the first
// "@", or after any suffix → "user@host:tests", "a@1b" or "x@10@4" red.
func TestBaseLabel(t *testing.T) {
	for in, want := range map[string]string{
		"proj:pytest@10":  "proj:pytest",
		"x@-":             "x",
		"user@host:tests": "user@host:tests",
		"a@1b":            "a@1b",
		"@5":              "",
		"plain":           "plain",
		"x@10@4":          "x@10",
		"x@":              "x@",
	} {
		if got := BaseLabel(in); got != want {
			t.Errorf("BaseLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLookupForms: cpu and mem as strings or TOML integers (mem in GiB). Mutation: read only
// strings → entry "b" red.
func TestLookupForms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "needs.toml")
	write(t, path, `
[needs."a"]
cpu = "1..7"
mem = "3.1G"
[needs."b"]
cpu = 4
mem = 3
[needs."c"]
cpu_per_worker = 0.4
light = true
[needs."d"]
pinned = true
mem = "2G"
[needs.""]
cpu = 3
`)
	f := load(t, path)
	for _, tc := range []struct {
		label string
		want  Entry
	}{
		{"a", Entry{CPU: &spec.Range{Min: 1, Max: 7}, MemMiB: 3175}},
		{"a@10", Entry{CPU: &spec.Range{Min: 1, Max: 7}, MemMiB: 3175}},
		{"b", Entry{CPU: &spec.Range{Min: 4, Max: 4}, MemMiB: 3072}},
		{"c", Entry{CPUPerWorker: 0.4}},
		{"d", Entry{MemMiB: 2048, Pinned: true}},
	} {
		got, ok, err := f.Lookup(tc.label)
		if err != nil || !ok {
			t.Errorf("Lookup(%q) = %v, %v", tc.label, ok, err)
			continue
		}
		if (got.CPU == nil) != (tc.want.CPU == nil) || (got.CPU != nil && *got.CPU != *tc.want.CPU) ||
			got.MemMiB != tc.want.MemMiB || got.Pinned != tc.want.Pinned || got.CPUPerWorker != tc.want.CPUPerWorker {
			t.Errorf("Lookup(%q) = %+v (cpu %v), want %+v (cpu %v)", tc.label, got, got.CPU, tc.want, tc.want.CPU)
		}
	}
	for _, label := range []string{"", "@5", "e"} {
		if _, ok, err := f.Lookup(label); ok || err != nil {
			t.Errorf("Lookup(%q) = %v, %v; want nothing", label, ok, err)
		}
	}
}

// TestLookupBadValue: a bad value is an error naming the label and the key. Mutation: drop the
// key from the message → red.
func TestLookupBadValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "needs.toml")
	write(t, path, "[needs.\"a\"]\ncpu = \"x\"\n")
	_, ok, err := load(t, path).Lookup("a")
	if ok || err == nil || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "cpu") {
		t.Fatalf("Lookup = %v, %v; want an error naming \"a\" and cpu", ok, err)
	}
}

func TestLoadMissing(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "needs.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.Lookup("a"); ok || err != nil {
		t.Fatalf("Lookup in a missing file = %v, %v", ok, err)
	}
}

// TestUpdateKeepsUnknownKeysAndPinned: an edit of one entry keeps the other tables and keys.
// Mutation: encode only the [needs] table → the gate key is lost.
func TestUpdateKeepsUnknownKeysAndPinned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "needs.toml")
	write(t, path, `
[gate]
budget = "x"
[needs."p"]
pinned = true
mem = "2G"
note = "mine"
`)
	err := Update(path, func(e map[string]map[string]any) error {
		e["q"] = map[string]any{"cpu": "2..4"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{"[gate]", `budget = "x"`, "pinned = true", `note = "mine"`, `cpu = "2..4"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("file lost %q:\n%s", want, data)
		}
	}
	if e, ok, err := load(t, path).Lookup("p"); !ok || err != nil || !e.Pinned || e.MemMiB != 2048 {
		t.Errorf("Lookup(p) = %+v, %v, %v", e, ok, err)
	}
}

// TestUpdateThroughSymlink: the file the link names is replaced and the link stays. Mutation:
// skip EvalSymlinks → the link is replaced by a regular file.
func TestUpdateThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	realPath, link := filepath.Join(dir, "real.toml"), filepath.Join(dir, "needs.toml")
	write(t, realPath, "[needs.\"a\"]\ncpu = 1\n")
	if err := os.Symlink(realPath, link); err != nil {
		t.Fatal(err)
	}
	err := Update(link, func(e map[string]map[string]any) error {
		e["b"] = map[string]any{"cpu": int64(2)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link after Update: %v, %v", fi, err)
	}
	if _, ok, _ := load(t, realPath).Lookup("b"); !ok {
		t.Fatal("the real file has no entry b")
	}
}

// TestUpdateCreatesPrivateFile. Mutation: create with 0644 → red.
func TestUpdateCreatesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "needs.toml")
	if err := Update(path, func(e map[string]map[string]any) error {
		e["a"] = map[string]any{"cpu": int64(1)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
}

// TestUpdateSerializes: concurrent edits of distinct entries all land. Mutation: skip the flock
// → entries lost (run with -count=5).
func TestUpdateSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "needs.toml")
	var wg sync.WaitGroup
	for g := range 2 {
		wg.Go(func() {
			for i := range 20 {
				label := fmt.Sprintf("g%d-%d", g, i)
				if err := Update(path, func(e map[string]map[string]any) error {
					e[label] = map[string]any{"cpu": int64(1)}
					return nil
				}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	f := load(t, path)
	for g := range 2 {
		for i := range 20 {
			if _, ok, _ := f.Lookup(fmt.Sprintf("g%d-%d", g, i)); !ok {
				t.Errorf("entry g%d-%d lost", g, i)
			}
		}
	}
}

// TestPerWorkerEntryRequest: cpu_per_worker is cpu tokens per worker, so the label's @K, the
// entry's cpu or the CPU count gives the workers and the request is c tokens for each. Mutation:
// request the worker width itself as tokens → 8..8 for x@8, red; ceil without the epsilon →
// 11 becomes 12 for 1.1 x 10, red.
func TestPerWorkerEntryRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "needs.toml")
	write(t, path, `
[needs."x"]
cpu_per_worker = 0.4
[needs."ranged"]
cpu = "2..12"
cpu_per_worker = 0.4
[needs."quarter"]
cpu_per_worker = 0.25
[needs."whole"]
cpu_per_worker = 2
[needs."noise"]
cpu_per_worker = 1.1
[needs."wide"]
cpu_per_worker = 100
[needs."plain"]
cpu = "3..4"
`)
	f := load(t, path)
	for _, tc := range []struct {
		name, label  string
		cpus         int
		tokens, wide spec.Range
	}{
		{"label count", "x@8", 64, spec.Range{Min: 4, Max: 4}, spec.Range{Min: 8, Max: 8}},
		{"label count beats the entry's range", "ranged@8", 64, spec.Range{Min: 4, Max: 4}, spec.Range{Min: 8, Max: 8}},
		{"entry's range", "ranged", 64, spec.Range{Min: 1, Max: 5}, spec.Range{Min: 2, Max: 12}},
		{"CPU count", "quarter", 10, spec.Range{Min: 1, Max: 3}, spec.Range{Min: 1, Max: 10}},
		{"CPU count below 1", "quarter", 0, spec.Range{Min: 1, Max: 1}, spec.Range{Min: 1, Max: 1}},
		{"integer c", "whole@3", 64, spec.Range{Min: 6, Max: 6}, spec.Range{Min: 3, Max: 3}},
		{"float noise", "noise@10", 64, spec.Range{Min: 11, Max: 11}, spec.Range{Min: 10, Max: 10}},
		{"clamped to the cap", "wide@10", 64, spec.Range{Min: 256, Max: 256}, spec.Range{Min: 10, Max: 10}},
		{"@- is no count", "quarter@-", 10, spec.Range{Min: 1, Max: 3}, spec.Range{Min: 1, Max: 10}},
		{"@0 is no count", "quarter@0", 10, spec.Range{Min: 1, Max: 3}, spec.Range{Min: 1, Max: 10}},
	} {
		e, ok, err := f.Lookup(tc.label)
		if err != nil || !ok {
			t.Fatalf("%s: Lookup(%q) = %v, %v", tc.name, tc.label, ok, err)
		}
		tokens, wide := e.Request(WorkersFromLabel(tc.label), tc.cpus)
		if tokens != tc.tokens || wide != tc.wide {
			t.Errorf("%s: Request = tokens %v, width %v; want %v, %v", tc.name, tokens, wide, tc.tokens, tc.wide)
		}
	}
}

// TestCPUPerWorkerMustBePositive: zero, a negative number and a non-number are errors naming
// the key. Mutation: accept 0 → red.
func TestCPUPerWorkerMustBePositive(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"0", "want a number above 0"},
		{"0.0", "want a number above 0"},
		{"-1", "want a number above 0"},
		{`"2"`, "want a number"},
		{"true", "want a number"},
	} {
		path := filepath.Join(t.TempDir(), "needs.toml")
		write(t, path, "[needs.\"a\"]\ncpu_per_worker = "+tc.value+"\n")
		_, ok, err := load(t, path).Lookup("a")
		if ok || err == nil || !strings.Contains(err.Error(), "cpu_per_worker") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("cpu_per_worker = %s: Lookup = %v, %v; want an error containing %q", tc.value, ok, err, tc.want)
		}
	}
}

// TestWorkersFromLabel: the final @digits, as BaseLabel strips it. Mutation: read the first @ → red.
func TestWorkersFromLabel(t *testing.T) {
	for label, want := range map[string]int{"x@8": 8, "x": 0, "x@-": 0, "x@": 0, "a@2@16": 16, "x@y": 0, "x@08": 8, "@5": 5} {
		if got := WorkersFromLabel(label); got != want {
			t.Errorf("WorkersFromLabel(%q) = %d, want %d", label, got, want)
		}
	}
}
