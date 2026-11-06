package main

import (
	"errors"
	"slices"
	"strings"
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
		o, err := parseRun([]string{"--mem", c.arg, "--", "true"}, IO{})
		if err != nil {
			t.Fatalf("--mem %s: %v", c.arg, err)
		}
		o.resolveNeed(nil, noLearned, 32768)
		if o.mem != c.mib {
			t.Errorf("--mem %s: mem = %d, want %d", c.arg, o.mem, c.mib)
		}
		if got := o.needText(); got != c.text {
			t.Errorf("--mem %s: needText = %q, want %q", c.arg, got, c.text)
		}
	}
	for _, arg := range []string{"0", "0M", "x", "-1G", "3 G"} {
		_, err := parseRun([]string{"--mem", arg, "--", "true"}, IO{})
		var ue *spec.UsageError
		if !errors.As(err, &ue) {
			t.Errorf("--mem %q: parseRun = %v, want a usage error", arg, err)
		}
	}
}

// A brace in an argument is the command's own.
// Mutation: substitute {n} in parseRun's argv, and the case reddens.
func TestBracesReachTheChild(t *testing.T) {
	o, err := parseRun([]string{"--", "echo", "{n}", "-j{n}", "{n:mem}"}, IO{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"echo", "{n}", "-j{n}", "{n:mem}"}; !slices.Equal(o.argv, want) {
		t.Errorf("argv = %q, want %q", o.argv, want)
	}
}

// A flag the command does not have is a usage error, whatever it is called.
func TestUnknownFlagIsUsage(t *testing.T) {
	for _, flag := range []string{"--nonesuch", "--nonesuch=1", "-x"} {
		_, err := parseRun([]string{flag, "--", "true"}, IO{})
		var ue *spec.UsageError
		if !errors.As(err, &ue) {
			t.Errorf("parseRun(%s) = %v, want a usage error", flag, err)
		}
	}
}

// --restart takes yes or no and nothing else; given twice, the last one counts.
// Mutation: accept any value, and the "maybe" case reddens.
func TestRestartFlag(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--restart=yes"}, "yes"},
		{[]string{"--restart", "no"}, "no"},
		{[]string{"--restart=yes", "--restart=no"}, "no"},
		{nil, ""},
	} {
		o, err := parseRun(append(c.args, "--", "true"), IO{})
		if err != nil || o.restart != c.want {
			t.Errorf("%q: restart %q, err %v; want %q", c.args, o.restart, err, c.want)
		}
	}
	for _, arg := range []string{"--restart=maybe", "--restart=", "--restart=YES", "--restart=true"} {
		_, err := parseRun([]string{arg, "--", "true"}, IO{})
		var ue *spec.UsageError
		if !errors.As(err, &ue) {
			t.Errorf("%s: parseRun = %v, want a usage error", arg, err)
		}
	}
}

// The choice is required only of a run that could be a victim: its command has a group of its
// own and its label is not no_restart, which also wins over --restart=yes.
// Mutations: dropping the shared test requires it in a terminal; letting --restart=yes win over
// no_restart makes the marked label restartable.
func TestRestartRequiredOnlyOfAVictim(t *testing.T) {
	for _, c := range []struct {
		o                          runOptions
		shared, missing, restartOK bool
	}{
		{runOptions{}, false, true, false},
		{runOptions{}, true, false, false},
		{runOptions{noRestart: true}, false, false, false},
		{runOptions{restart: "yes"}, false, false, true},
		{runOptions{restart: "no"}, false, false, false},
		{runOptions{restart: "yes", noRestart: true}, false, false, false},
	} {
		if got := c.o.restartMissing(c.shared); got != c.missing {
			t.Errorf("%+v shared %v: missing %v, want %v", c.o, c.shared, got, c.missing)
		}
		if got := c.o.restartable(); got != c.restartOK {
			t.Errorf("%+v: restartable %v, want %v", c.o, got, c.restartOK)
		}
	}
}

// The help says what each value means in plain words, and the missing-flag error says what to
// pick. Mutation: dropping the "When unsure" line from either reddens it.
func TestRestartHelpAndError(t *testing.T) {
	for name, s := range map[string]string{"help": runHelp, "error": errRestartMissing.Error()} {
		for _, want := range []string{"yes", "no", "tests, builds, linters, type checks",
			"databases", "migrations", "deploys", "pushes", "sending messages", "writing shared files", "When"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s lacks %q:\n%s", name, want, s)
			}
		}
		if !strings.Contains(s, "unsure, no") && !strings.Contains(s, "unsure, --restart=no") {
			t.Errorf("%s does not say to pick no when unsure:\n%s", name, s)
		}
	}
}

// A command is container work when argv[0] is a container program by its basename; a wrapper
// (sh -c, env, time) is not. Mutation: look at every argument, and the sh -c row reddens.
func TestContainerCommand(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"docker", "run"}, true},
		{[]string{"/usr/local/bin/docker", "run"}, true},
		{[]string{"make", "test"}, false},
		{[]string{"sh", "-c", "docker run"}, false},
		{[]string{"/x/dockerd"}, false},
		{nil, false},
	} {
		if got := (runOptions{argv: c.argv}).containerCommand(); got != c.want {
			t.Errorf("containerCommand(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
}
