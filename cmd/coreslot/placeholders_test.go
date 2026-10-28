package main

import (
	"slices"
	"testing"

	"github.com/core-slot/coreslot/internal/needs"
	"github.com/core-slot/coreslot/internal/spec"
)

func TestSubstitute(t *testing.T) {
	grant := map[string]int{"cpu": 6, "pg-local": 1}
	cases := []struct {
		name   string
		argv   []string
		inside bool
		want   []string
	}{
		{"exact cpu", []string{"pytest", "-n", "{n}"}, false, []string{"pytest", "-n", "6"}},
		{"exact resource", []string{"x", "{n:pg-local}"}, false, []string{"x", "1"}},
		{"escape", []string{"echo", "{{n}}"}, false, []string{"echo", "{n}"}},
		{"escape resource", []string{"echo", "{{n:pg-local}}"}, false, []string{"echo", "{n:pg-local}"}},
		{"inside without --subst", []string{"make", "-j{n}"}, false, []string{"make", "-j{n}"}},
		{"inside with --subst", []string{"make", "-j{n}"}, true, []string{"make", "-j6"}},
		{"several inside", []string{"x", "a{n}b{n:pg-local}c{{n}}"}, true, []string{"x", "a6b1c{n}"}},
		{"not a placeholder", []string{"echo", "{n: 1}"}, false, []string{"echo", "{n: 1}"}},
		{"not a placeholder with --subst", []string{"echo", "'{n: 1}'"}, true, []string{"echo", "'{n: 1}'"}},
		{"command itself", []string{"{n}"}, false, []string{"6"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := substitute(c.argv, grant, c.inside, 0, spec.Range{})
			if !slices.Equal(got, c.want) {
				t.Fatalf("substitute(%q, inside=%v) = %q, want %q", c.argv, c.inside, got, c.want)
			}
		})
	}
}

func TestPlaceholderResources(t *testing.T) {
	cases := []struct {
		argv   []string
		inside bool
		want   []string
	}{
		{[]string{"x", "{n}", "{n:pg-local}"}, false, []string{"cpu", "pg-local"}},
		{[]string{"x", "{{n:gpu}}"}, false, nil},
		{[]string{"make", "-j{n:gpu}"}, false, nil},
		{[]string{"make", "-j{n:gpu}"}, true, []string{"gpu"}},
	}
	for _, c := range cases {
		if got := placeholderResources(c.argv, c.inside); !slices.Equal(got, c.want) {
			t.Errorf("placeholderResources(%q, %v) = %q, want %q", c.argv, c.inside, got, c.want)
		}
	}
}

// {n:mem} is the mem grant in MiB, as CORESLOT_GRANTED_MEM is. Mutation: substituting the token
// count turns it red.
func TestSubstituteMemMiB(t *testing.T) {
	got := substitute([]string{"x", "{n:mem}", "--mem={n:mem}M"}, map[string]int{"cpu": 2, "mem": 24}, true, 0, spec.Range{})
	if want := []string{"x", "3072", "--mem=3072M"}; !slices.Equal(got, want) {
		t.Errorf("substitute = %q, want %q", got, want)
	}
}

// {n} is the worker count where the request is per worker: the most workers the grant pays for
// at cpu_per_worker each, at most the width's max and at least 1, and the grant itself where it
// is not. A grant below the width's min (clamped by the budget, or a nested run's from its
// parent) gives the workers it pays for, never the min. Mutation: substitute the grant for a
// per-worker request → 5, 1 and 3, red; clamp to the width's min → 16, 6, 12 and 2, red; drop
// the floor of 1 → 0, red; drop the upper clamp → 15, red; drop the epsilon → 9, red.
func TestSubstituteWorkers(t *testing.T) {
	saved := machineCPUs
	t.Cleanup(func() { machineCPUs = saved })
	machineCPUs = func() (int, error) { return 64, nil }
	argv := []string{"x", "{n}"}
	w := spec.Range{Min: 2, Max: 12}
	cases := []struct {
		name      string
		grant     int
		perWorker float64
		width     spec.Range
		want      string
	}{
		{"grant buys the width's last worker", 5, 0.4, w, "12"},
		{"grant buys more workers than the width", 6, 0.4, w, "12"},
		{"grant buys the width's first worker", 1, 0.4, w, "2"},
		{"grant inside the width", 3, 0.4, w, "7"},
		{"float quotient on a whole number", 4, 0.4, w, "10"},
		{"not per worker", 3, 0, w, "3"},
		{"budget clamped an @16 request", 8, 1, spec.Range{Min: 16, Max: 16}, "8"},
		{"budget clamped a range request", 8, 2, spec.Range{Min: 6, Max: 10}, "4"},
		{"nested grant below an @12 request", 2, 0.35, spec.Range{Min: 12, Max: 12}, "5"},
		{"grant below one worker", 1, 1.5, w, "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := substitute(argv, map[string]int{"cpu": c.grant}, false, c.perWorker, c.width)
			if want := []string{"x", c.want}; !slices.Equal(got, want) {
				t.Errorf("substitute = %q, want %q", got, want)
			}
		})
	}
	// --need cpu on the command line wins: the entry's c and width are not kept, and {n} is the
	// token count.
	lookup := func(string) (needs.Entry, bool, func(error)) {
		return needs.Entry{CPUPerWorker: 0.4}, true, func(error) {}
	}
	o, err := parseRun([]string{"--label", "x@8", "--need", "cpu=3", "--", "x", "{n}"}, IO{}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	got := substitute(o.argv, map[string]int{"cpu": 3}, o.subst, o.cpuPerWorker, o.workers)
	if want := []string{"x", "3"}; !slices.Equal(got, want) {
		t.Errorf("--need cpu=3: substitute = %q, want %q", got, want)
	}
}

// {n:cpu} is the worker count too, and {n:mem} and escapes are untouched. Mutation: apply the
// worker count to every resource → mem 1, red.
func TestSubstituteWorkersOnlyCPU(t *testing.T) {
	got := substitute([]string{"x", "{n:cpu}", "{n:mem}", "{{n}}"}, map[string]int{"cpu": 4, "mem": 2}, false, 0.5, spec.Range{Min: 1, Max: 64})
	if want := []string{"x", "8", "256", "{n}"}; !slices.Equal(got, want) {
		t.Errorf("substitute = %q, want %q", got, want)
	}
}
