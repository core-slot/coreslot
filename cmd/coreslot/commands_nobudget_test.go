package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// On Windows status, cancel and doctor have no budget to work on, yet --help still prints the
// command's usage on stdout and exits 0 (PROTOCOL.md section 19), recognized by the same flag set
// as on unix; everything else exits 70. noBudget is platform-neutral, so this runs everywhere.
// Mutation: return exitInternal for every invocation in noBudget and the help cases turn red.
func TestNoBudgetAnswersOnlyHelp(t *testing.T) {
	commands := []struct {
		name, usage string
		flags       func() *flag.FlagSet
	}{
		{"status", statusUsage, func() *flag.FlagSet { fs, _, _ := statusFlags(); return fs }},
		{"cancel", cancelUsage, func() *flag.FlagSet { fs, _ := cancelFlags(); return fs }},
		{"doctor", doctorUsage, func() *flag.FlagSet { fs, _, _ := doctorFlags(); return fs }},
	}
	cases := []struct {
		args []string
		help bool
	}{
		{[]string{"--help"}, true},
		{[]string{"-h"}, true},
		{[]string{"-help"}, true},
		{[]string{"--h"}, true},
		{nil, false},
		{[]string{"x"}, false},
		{[]string{"x", "--help"}, false},       // flags end at the first argument
		{[]string{"--", "--help"}, false},      // and at --
		{[]string{"--bogus", "--help"}, false}, // an unknown flag is a usage error on unix
	}
	for _, c := range commands {
		for _, tc := range cases {
			var code int
			stdout, stderr := capture(t, func(io IO) { code = noBudget(c.name, c.usage, c.flags(), tc.args, io) })
			want := struct {
				code           int
				stdout, stderr string
			}{0, c.usage + "\n", ""}
			if !tc.help {
				want.code, want.stdout, want.stderr = exitInternal, "", "coreslot: "+c.name+": Windows has no coreslot budget\n"
			}
			if code != want.code || stdout != want.stdout || stderr != want.stderr {
				t.Errorf("%s %q: exit %d, stdout %q, stderr %q; want %d, %q, %q",
					c.name, tc.args, code, stdout, stderr, want.code, want.stdout, want.stderr)
			}
		}
	}

	// The command's own flags take part as on unix: one of them before --help still asks for
	// help, and a flag's value is not a request for help.
	for _, c := range []struct {
		name string
		fs   *flag.FlagSet
		args []string
		want int
	}{
		{"status", func() *flag.FlagSet { fs, _, _ := statusFlags(); return fs }(), []string{"--json", "--help"}, 0},
		{"doctor", func() *flag.FlagSet { fs, _, _ := doctorFlags(); return fs }(), []string{"--reset", "-h"}, 0},
		{"cancel", func() *flag.FlagSet { fs, _ := cancelFlags(); return fs }(), []string{"--label", "--help"}, exitInternal},
	} {
		var code int
		capture(t, func(io IO) { code = noBudget(c.name, "usage", c.fs, c.args, io) })
		if code != c.want {
			t.Errorf("%s %q: exit %d, want %d", c.name, c.args, code, c.want)
		}
	}
}

// capture runs f with stdout and stderr going to files and returns what each received.
func capture(t *testing.T, f func(IO)) (stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	var out [2]*os.File
	for i, name := range []string{"stdout", "stderr"} {
		file, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		out[i] = file
	}
	f(IO{Stdin: os.Stdin, Stdout: out[0], Stderr: out[1]})
	var got [2]string
	for i, file := range out {
		b, err := os.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		got[i] = string(b)
	}
	return got[0], got[1]
}
