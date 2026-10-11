package main

import (
	"slices"
	"testing"
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
			got := substitute(c.argv, grant, c.inside)
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
