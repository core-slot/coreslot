//go:build unix

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
)

// The usage hint names the mismatch and the request that would fit. Mutations: dropping the
// hintAfter check turns the 30 s row red; `used < lightBelow` written `<=` the 0.25-core row;
// `used-float64(tokens) >= 1` dropped the 1.6-on-1 row.
func TestUsageHint(t *testing.T) {
	for _, c := range []struct {
		name       string
		label      string
		tokens     int
		cpuS, ranS float64
		want       string // "" for no hint
	}{
		{"a stream on 8 tokens", "dna:annotate@-", 8, 0.15 * 4142, 4142,
			"dna:annotate@- used 0.15 of 8 cpu over 1h09m02s; --light runs a command this light without waiting"},
		{"a tool on 8 tokens", "dna:annotate@-", 8, 1.15 * 4074, 4074,
			"dna:annotate@- used 1.15 of 8 cpu over 1h07m54s; --need cpu=2 would fit"},
		{"a run that fits", "", 8, 6 * 600, 600, ""},
		{"exactly half fits", "", 8, 4 * 600, 600, ""},
		{"a short run says nothing", "", 8, 0.1 * 30, 30, ""},
		{"0.25 cores is not light", "", 1, 0.25 * 600, 600, ""},
		{"too few tokens", "", 1, 3.2 * 600, 600, "the command used 3.20 cpu on 1 over 10m00s; --need cpu=4 would cover it"},
		{"a little over is noise", "", 1, 1.6 * 600, 600, ""},
		{"a light run that was not", "dl", 0, 3.2 * 600, 600,
			"dl used 3.20 cpu outside the budget over 10m00s; --need cpu=4 would count it"},
		{"a light run that was", "dl", 0, 0.1 * 600, 600, ""},
	} {
		if got := usageHint(c.label, c.tokens, c.cpuS, c.ranS); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The memory hint names a peak that outgrew the mem grant by more than one token, or a big peak
// with no mem need, and the suggest command for the label's base. Mutation: comparing the peak
// with the grant without the one-token slack turns the 2.1 GiB row red.
func TestMemHint(t *testing.T) {
	const gib = 1024 * 1024 // KiB
	for _, c := range []struct {
		label    string
		grantMiB int64
		clamped  bool
		peakKiB  int64
		ranS     float64
		want     string // "" for no hint
		not      string
	}{
		{"a", 2048, false, 5 * gib / 2, 3, "peaked at 2.5 GiB on a 2 GiB mem grant; coreslot suggest --write --label a", ""},
		{"a", 2048, false, 21 * gib / 10, 3, "", ""},
		{"a@10", 0, false, 3 * gib / 2, 3, "peaked at 1.5 GiB with no mem need; coreslot suggest --write --label a", ""},
		{"a", 0, false, 900 * 1024, 3, "", ""},
		{"a", 9728, true, 11 * gib, 3, "clamped to the budget's 9.5 GiB", "suggest"},
		{"a", 0, false, 3 * gib, 0.5, "", ""},
		{"", 0, false, 3 * gib, 3, "", ""},
	} {
		got := memHint(c.label, c.grantMiB, c.clamped, c.peakKiB, c.ranS)
		if c.want == "" && got != "" || !strings.Contains(got, c.want) || c.not != "" && strings.Contains(got, c.not) {
			t.Errorf("memHint(%q, %d, %v, %d, %v) = %q, want it to contain %q", c.label, c.grantMiB, c.clamped, c.peakKiB, c.ranS, got, c.want)
		}
	}
}

// Only a run that sized its own request is told how to size it. Mutation: treating every
// failOpen reason as light turns the timeout row red.
func TestHintTokens(t *testing.T) {
	grant := map[string]int{"cpu": 8}
	for _, c := range []struct {
		name   string
		c      child
		tokens int
		ok     bool
	}{
		{"admitted", child{grant: grant}, 8, true},
		{"light", child{grant: map[string]int{"cpu": 1}, failOpen: lightReason}, 0, true},
		{"timeout", child{grant: grant, failOpen: timeoutReason}, 0, false},
		{"fail-open", child{grant: grant, failOpen: "the state directory is missing"}, 0, false},
		{"pass-through", child{grant: grant, nested: true}, 0, false},
	} {
		if tokens, ok := c.c.hintTokens(); tokens != c.tokens || ok != c.ok {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", c.name, tokens, ok, c.tokens, c.ok)
		}
	}
}

// The waiting line shows a mem need in bytes, and a holder's mem grant too. Mutation: amount
// printing tokens for mem turns it red.
func TestWaitingLineMem(t *testing.T) {
	need := map[string]spec.Range{"cpu": {Min: 1, Max: 1}, "mem": {Min: 24, Max: 24}}
	holders := []engine.HolderInfo{{Label: "gate", PID: 4711, Grant: map[string]int{"cpu": 2, "mem": 8}}}
	r := &run{o: runOptions{need: need}}
	line := r.waitingLine(engine.Waiting{Position: 1}, holders, 0)
	for _, want := range []string{"waiting for cpu 1..1, mem 3 GiB: ", "gate pid 4711 cpu2 mem 1 GiB ("} {
		if !strings.Contains(line, want) {
			t.Errorf("waiting line %q, want %q", line, want)
		}
	}
}

// The first waiting line says when stdin is a pipe, and no later one does. Mutation: dropping
// the pipeNoted check turns the second-line check red; `== 0` written `== 1` the file row.
func TestPipeNote(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	defer wr.Close()
	file, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	need := map[string]spec.Range{"cpu": {Min: 8, Max: 8}}
	w := engine.Waiting{Position: 2}

	r := &run{o: runOptions{need: need}, io: IO{Stdin: rd}}
	if first := r.waitingLine(w, nil, 0); !strings.Contains(first, "; stdin is a pipe: ") {
		t.Errorf("first line with a pipe on stdin: %q", first)
	}
	if second := r.waitingLine(w, nil, 0); strings.Contains(second, "pipe") {
		t.Errorf("second line repeats the note: %q", second)
	}
	r = &run{o: runOptions{need: need}, io: IO{Stdin: file}}
	if line := r.waitingLine(w, nil, 0); strings.Contains(line, "pipe") {
		t.Errorf("a file on stdin got the pipe note: %q", line)
	}
}
