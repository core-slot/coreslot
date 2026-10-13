package main

import (
	"errors"
	"maps"
	"testing"

	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// A run without --need asks for cpu=1..16 when its command can use the grant through a cpu
// placeholder, and for cpu=1..1 otherwise; an explicit --need is never widened. Mutation: make
// defaultNeed return cpu=1..1 always, and every placeholder case reddens; make it ignore inside,
// and the --subst cases do.
func TestRunDefaultNeed(t *testing.T) {
	one := map[string]spec.Range{"cpu": {Min: 1, Max: 1}}
	wide := map[string]spec.Range{"cpu": {Min: 1, Max: 16}}
	cases := []struct {
		name string
		args []string
		want map[string]spec.Range
	}{
		{"no placeholder", []string{"--", "sh", "-c", "echo $CORESLOT_GRANTED_CPU"}, one},
		{"exact {n}", []string{"--", "pytest", "-n", "{n}"}, wide},
		{"exact {n:cpu}", []string{"--", "pytest", "-n", "{n:cpu}"}, wide},
		{"escaped {{n}}", []string{"--", "echo", "{{n}}"}, one},
		{"escaped {{n:cpu}}", []string{"--", "echo", "{{n:cpu}}"}, one},
		{"inside without --subst", []string{"--", "make", "-j{n}"}, one},
		{"inside with --subst", []string{"--subst", "--", "make", "-j{n}"}, wide},
		{"inside {n:cpu} with --subst", []string{"--subst", "--", "make", "-j{n:cpu}"}, wide},
		{"escaped inside with --subst", []string{"--subst", "--", "echo", "-j{{n}}"}, one},
		{"explicit need with {n}", []string{"--need", "cpu=2", "--", "pytest", "-n", "{n}"},
			map[string]spec.Range{"cpu": {Min: 2, Max: 2}}},
		{"one-sided min", []string{"--need", "cpu=4..", "--", "true"},
			map[string]spec.Range{"cpu": {Min: 4, Max: 16}}},
		{"one-sided max", []string{"--need", "cpu=..2", "--", "true"},
			map[string]spec.Range{"cpu": {Min: 1, Max: 2}}},
	}
	for _, c := range cases {
		o, err := parseRun(c.args, IO{})
		if err != nil {
			t.Errorf("%s: parseRun(%q): %v", c.name, c.args, err)
			continue
		}
		if !maps.Equal(o.need, c.want) {
			t.Errorf("%s: parseRun(%q) need = %v, want %v", c.name, c.args, o.need, c.want)
		}
	}
}

// The default never names a resource other than cpu, so a placeholder of another resource stays
// a usage error, with or without a cpu placeholder beside it.
func TestRunDefaultNeedKeepsPlaceholderCheck(t *testing.T) {
	for _, args := range [][]string{
		{"--", "echo", "{n:gpu}"},
		{"--", "echo", "{n}", "{n:gpu}"},
		{"--subst", "--", "make", "-j{n}", "-l{n:gpu}"},
	} {
		_, err := parseRun(args, IO{})
		var ue *spec.UsageError
		if !errors.As(err, &ue) {
			t.Errorf("parseRun(%q) = %v, want a usage error", args, err)
		}
	}
}

// Every malformed one-sided need is a usage error of parseRun, detected before anything queues.
func TestRunRejectsMalformedOneSidedNeed(t *testing.T) {
	for _, need := range []string{"cpu=..", "cpu=...", "cpu=-1..", "cpu=..0", "cpu=0..", "cpu=257..",
		"cpu=..257", "cpu= 4..", "cpu=4.. "} {
		_, err := parseRun([]string{"--need", need, "--", "true"}, IO{})
		var ue *spec.UsageError
		if !errors.As(err, &ue) {
			t.Errorf("--need %q: parseRun = %v, want a usage error", need, err)
		}
	}
}

// The waiting line shows the request as normalized, not as written.
func TestNeedTextShowsTheNormalizedRange(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--need", "cpu=4..", "--", "true"}, "cpu 4..16"},
		{[]string{"--need", "cpu=..2", "--need", "pg-local=1", "--", "true"}, "cpu 1..2, pg-local 1..1"},
		{[]string{"--", "echo", "{n}"}, "cpu 1..16"},
		{[]string{"--", "true"}, "cpu 1..1"},
	} {
		o, err := parseRun(c.args, IO{})
		if err != nil {
			t.Fatalf("parseRun(%q): %v", c.args, err)
		}
		if got := o.needText(); got != c.want {
			t.Errorf("parseRun(%q).needText() = %q, want %q", c.args, got, c.want)
		}
	}
}
