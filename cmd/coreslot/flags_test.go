package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/core-slot/coreslot/internal/spec"
)

// --mem is a size in MiB under spec.ParseBytes's rules, more than 0, which the request asks for.
// Mutation: accept 0, and the usage case reddens.
func TestMemFlag(t *testing.T) {
	for _, c := range []struct {
		arg  string
		mib  int64
		text string
	}{
		{"3G", 3072, "mem 3 GiB"},
		{"768M", 768, "mem 768 MiB"},
		{"2.5G", 2560, "mem 2.5 GiB"},
	} {
		o, err := parseRun([]string{"--mem", c.arg, "--", "true"}, IO{}, nil)
		if err != nil {
			t.Fatalf("--mem %s: %v", c.arg, err)
		}
		if o.mem != c.mib {
			t.Errorf("--mem %s: mem = %d, want %d", c.arg, o.mem, c.mib)
		}
		if got := o.needText(); got != c.text {
			t.Errorf("--mem %s: needText = %q, want %q", c.arg, got, c.text)
		}
	}
	for _, arg := range []string{"0", "0M", "x", "-1G", "3 G"} {
		_, err := parseRun([]string{"--mem", arg, "--", "true"}, IO{}, nil)
		var ue *spec.UsageError
		if !errors.As(err, &ue) {
			t.Errorf("--mem %q: parseRun = %v, want a usage error", arg, err)
		}
	}
}

// A run without --mem asks for the default 128 MiB, and a brace in an argument is the command's own.
// Mutation: substitute {n} in parseRun's argv, and the case reddens.
func TestBracesReachTheChild(t *testing.T) {
	o, err := parseRun([]string{"--", "echo", "{n}", "-j{n}", "{n:mem}"}, IO{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"echo", "{n}", "-j{n}", "{n:mem}"}; !slices.Equal(o.argv, want) {
		t.Errorf("argv = %q, want %q", o.argv, want)
	}
	if got := o.needText(); got != "mem 128 MiB" {
		t.Errorf("needText = %q, want mem 128 MiB", got)
	}
}

// A flag the command does not have is a usage error, whatever it is called.
func TestUnknownFlagIsUsage(t *testing.T) {
	for _, flag := range []string{"--nonesuch", "--nonesuch=1", "-x"} {
		_, err := parseRun([]string{flag, "--", "true"}, IO{}, nil)
		var ue *spec.UsageError
		if !errors.As(err, &ue) {
			t.Errorf("parseRun(%s) = %v, want a usage error", flag, err)
		}
	}
}
