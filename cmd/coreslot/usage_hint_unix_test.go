//go:build unix

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
)

// The memory hint names a peak that outgrew the mem grant by more than one token, or a big peak
// with no mem need, and the stats command for the label. Mutation: comparing the peak
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
		{"a", 2048, false, 5 * gib / 2, 3, "peaked at 2.5 GiB on a 2 GiB mem grant; coreslot stats --label a", ""},
		{"a", 2048, false, 21 * gib / 10, 3, "", ""},
		{"a", 0, false, 3 * gib / 2, 3, "peaked at 1.5 GiB with no mem need; coreslot stats --label a", ""},
		{"a", 0, false, 900 * 1024, 3, "", ""},
		{"a", 9728, true, 11 * gib, 3, "clamped to the budget's 9.5 GiB", "stats"},
		{"a", 0, false, 3 * gib, 0.5, "", ""},
		{"", 0, false, 3 * gib, 3, "", ""},
	} {
		got := memHint(c.label, c.grantMiB, c.clamped, c.peakKiB, c.ranS)
		if c.want == "" && got != "" || !strings.Contains(got, c.want) || c.not != "" && strings.Contains(got, c.not) {
			t.Errorf("memHint(%q, %d, %v, %d, %v) = %q, want it to contain %q", c.label, c.grantMiB, c.clamped, c.peakKiB, c.ranS, got, c.want)
		}
	}
}

// The waiting line shows a mem need in bytes, and a holder's mem grant too. Mutation: amount
// printing tokens for mem turns it red.
func TestWaitingLineMem(t *testing.T) {
	need := map[string]spec.Range{"mem": {Min: 24, Max: 24}}
	holders := []engine.HolderInfo{{Label: "gate", PID: 4711, Grant: map[string]int{"mem": 8}}}
	r := &run{o: runOptions{need: need}}
	line := r.waitingLine(engine.Waiting{Position: 1}, holders, 0)
	for _, want := range []string{"waiting for mem 3 GiB: ", "gate pid 4711 mem 1 GiB ("} {
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
	need := map[string]spec.Range{"mem": {Min: 8, Max: 8}}
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
