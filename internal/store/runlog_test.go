//go:build unix

package store

import (
	"bufio"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Nezhinskiy/coreslot/internal/spec"
)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestRunLogLineLimitAndRotation(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	logPath := filepath.Join(dir, LogDir, RunLog)
	r := RunRecord{
		TimeNS:              1791720000000000000,
		Label:               "gate:unit\nsecond line \x1b[31mred\x1b[0m",
		Need:                map[string]spec.Range{"cpu": {Min: 4, Max: 12}, "pg-local": {Min: 1, Max: 1}},
		Grant:               map[string]int{"cpu": 10, "pg-local": 1},
		Clamped:             []string{"cpu"},
		UnbudgetedResources: nil,
		WaitedS:             1.5,
		RanS:                12.25,
		Exit:                0,
		Load1:               3.5,
	}
	if err := s.AppendRun(r); err != nil {
		t.Fatal(err)
	}
	long := r
	long.FailOpen = strings.Repeat("é", 4000) // 8000 bytes of reason
	if err := s.AppendRun(long); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, logPath)
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2: %q", len(lines), lines)
	}
	wantKeys := []string{"clamped", "exit", "fail_open", "grant", "label", "load1", "need", "ran_s",
		"time_ns", "unbudgeted_resources", "waited_s"}
	for i, line := range lines {
		if len(line)+1 > MaxRunLine {
			t.Errorf("line %d is %d bytes with its LF, more than %d", i, len(line)+1, MaxRunLine)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if keys := slices.Sorted(maps.Keys(m)); !slices.Equal(keys, wantKeys) {
			t.Errorf("line %d keys %v, want exactly %v (never argv or environment)", i, keys, wantKeys)
		}
		if m["label"] != r.Label {
			t.Errorf("line %d label %q, want %q", i, m["label"], r.Label)
		}
		if need, _ := json.Marshal(m["need"]); string(need) != `{"cpu":[4,12],"pg-local":[1,1]}` {
			t.Errorf("need %s", need)
		}
		if ub, ok := m["unbudgeted_resources"].([]any); !ok || len(ub) != 0 {
			t.Errorf("unbudgeted_resources %v, want []", m["unbudgeted_resources"])
		}
	}
	var second map[string]any
	_ = json.Unmarshal([]byte(lines[1]), &second)
	fo, _ := second["fail_open"].(string)
	if fo == "" || !strings.HasPrefix(long.FailOpen, fo) || !utf8.ValidString(fo) {
		t.Errorf("fail_open was not truncated to a valid prefix: %d bytes", len(fo))
	}
	tooLong := r
	tooLong.Label = strings.Repeat("\x01", 128)
	tooLong.Need = map[string]spec.Range{}
	for _, c := range "abcdefghijklmnopqrstuvwxyz" {
		tooLong.Need[strings.Repeat(string(c), 32)] = spec.Range{Min: 256, Max: 256}
	}
	tooLong.Grant = map[string]int{}
	for name := range tooLong.Need {
		tooLong.Grant[name] = 256
		tooLong.Clamped = append(tooLong.Clamped, name)
		tooLong.UnbudgetedResources = append(tooLong.UnbudgetedResources, name)
	}
	if err := s.AppendRun(tooLong); err == nil {
		t.Error("AppendRun wrote a record that cannot fit 4 KiB")
	}
	if got := readLines(t, logPath); len(got) != 2 {
		t.Errorf("a refused record left %d lines", len(got))
	}

	// Rotation at 10 MiB, inside the section, keeping one generation.
	sec := enter(t, s)
	if err := os.WriteFile(filepath.Join(dir, LogDir, RunLogOld), []byte("previous generation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(logPath, RotateRunLogAt-1); err != nil {
		t.Fatal(err)
	}
	if s.RunLogNeedsRotation() {
		t.Error("RunLogNeedsRotation below 10 MiB")
	}
	if err := sec.RotateLogIfNeeded(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(logPath); fi == nil || fi.Size() != RotateRunLogAt-1 {
		t.Fatal("the log was rotated below 10 MiB")
	}
	if err := os.Truncate(logPath, RotateRunLogAt); err != nil {
		t.Fatal(err)
	}
	if !s.RunLogNeedsRotation() {
		t.Error("RunLogNeedsRotation at 10 MiB = false")
	}
	if err := sec.RotateLogIfNeeded(); err != nil {
		t.Fatal(err)
	}
	if exists(logPath) {
		t.Error("runs.jsonl still exists after rotation")
	}
	if fi, err := os.Stat(filepath.Join(dir, LogDir, RunLogOld)); err != nil || fi.Size() != RotateRunLogAt {
		t.Errorf("runs.1.jsonl after rotation: %v, %v; want the rotated 10 MiB generation", fi, err)
	}
	if err := s.AppendRun(r); err != nil {
		t.Fatal(err)
	}
	if got := readLines(t, logPath); len(got) != 1 {
		t.Errorf("a fresh runs.jsonl has %d lines", len(got))
	}
	// A reset removes log/; the next append recreates it.
	sec.Leave()
	if err := os.RemoveAll(filepath.Join(dir, LogDir)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRun(r); err != nil {
		t.Fatalf("AppendRun after log/ was removed: %v", err)
	}
	if fi, _ := os.Lstat(filepath.Join(dir, LogDir)); fi == nil || fi.Mode() != os.ModeDir|0o700 {
		t.Errorf("recreated log/: %v", fi)
	}
}

// recordDegraded records reason from outside the section through inSideSection, the path
// ClearDegraded takes, so that its format refusal and its bounded wait are covered as well.
func recordDegraded(s *Store, reason string) error {
	return s.inSideSection(func(sec *Section) error { return sec.RecordDegraded(reason) })
}

func TestDegradedRecordCountsAndClears(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	if d, err := s.Degraded(); d != nil || err != nil {
		t.Fatalf("Degraded with no record = %+v, %v", d, err)
	}
	before := NowNS()
	if err := recordDegraded(s, "state: refused filesystem"); err != nil {
		t.Fatal(err)
	}
	d, err := s.Degraded()
	if err != nil || d == nil {
		t.Fatalf("Degraded = %+v, %v", d, err)
	}
	if d.Reason != "state: refused filesystem" || d.Count != 1 || d.FirstNS < before || d.FirstNS > NowNS() || d.Malformed {
		t.Errorf("first record %+v", d)
	}
	first := d.FirstNS
	if err := recordDegraded(s, "state: inode mismatch"); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Degraded()
	if d.Reason != "state: inode mismatch" || d.Count != 2 || d.FirstNS != first {
		t.Errorf("second record %+v, want the new reason, count 2, first_ns %d", d, first)
	}
	if fi, _ := os.Lstat(filepath.Join(dir, DegradedFile)); fi.Mode() != 0o600 {
		t.Errorf("degraded mode %v", fi.Mode())
	}
	if err := recordDegraded(s, strings.Repeat("\x01", 9000)); err != nil {
		t.Fatal(err)
	}
	if d, err := s.Degraded(); err != nil || d.Malformed || d.Count != 3 || !strings.HasPrefix(strings.Repeat("\x01", 9000), d.Reason) {
		t.Errorf("a record with an oversized reason reads back as %+v, %v; want a shortened reason, count 3", d, err)
	}
	if err := s.ClearDegraded(); err != nil {
		t.Fatal(err)
	}
	if d, err := s.Degraded(); d != nil || err != nil {
		t.Errorf("Degraded after ClearDegraded = %+v, %v", d, err)
	}
	if err := s.ClearDegraded(); err != nil {
		t.Errorf("ClearDegraded with no record: %v", err)
	}

	// A malformed record still reports degraded mode; the next record starts afresh.
	if err := os.WriteFile(filepath.Join(dir, DegradedFile), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err = s.Degraded()
	if err != nil || d == nil || !d.Malformed {
		t.Fatalf("Degraded over a malformed record = %+v, %v; want Malformed", d, err)
	}
	if err := recordDegraded(s, "again"); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.Degraded(); d.Count != 1 || d.Reason != "again" || d.Malformed {
		t.Errorf("record over a malformed one %+v", d)
	}

	// Under a format this client does not implement, nothing is written.
	if err := os.WriteFile(filepath.Join(dir, FormatFile), []byte("coreslot-protocol 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := snapshot(t, dir)
	if err := recordDegraded(s, "format"); err == nil {
		t.Error("RecordDegraded wrote under an unknown format")
	}
	if err := s.ClearDegraded(); err == nil {
		t.Error("ClearDegraded changed a directory of an unknown format")
	}
	if after := snapshot(t, dir); after != snap {
		t.Errorf("the directory changed under an unknown format:\n%s\n%s", snap, after)
	}
}

func TestRecordDegradedGivesUpOnABusySection(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	startHelper(t, "section", dir, "", nil)
	start := time.Now()
	err := recordDegraded(s, "busy")
	if err == nil {
		t.Fatal("RecordDegraded succeeded while another process holds the section")
	}
	if d := time.Since(start); d < sideSectionWait || d > sideSectionWait+3*time.Second {
		t.Errorf("RecordDegraded gave up after %v, want about %v", d, sideSectionWait)
	}
	if exists(filepath.Join(dir, DegradedFile)) {
		t.Error("a degraded record was written without the section")
	}
}

// writeOnce makes one write(2): on a non-blocking pipe nobody reads, a 1 MiB write returns
// short at once, where a looping write would block until the pipe is read.
func TestWriteOnceIssuesOneWrite(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	b := make([]byte, 1<<20)
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := writeOnce(w, b)
		done <- result{n, err}
	}()
	select {
	case res := <-done:
		if res.err != nil || res.n <= 0 || res.n >= len(b) {
			t.Errorf("writeOnce = %d, %v; want one short write", res.n, res.err)
		}
	case <-time.After(10 * time.Second):
		r.Close() // unblock the writer
		t.Fatal("writeOnce did not return: it wrote more than once")
	}
}

// PROTOCOL.md section 20: a pass-through run's line carries "nested": true; every other line
// has no such key (the key is encoded only when true).
func TestRunLogNestedMarkerIsEncodedOnlyWhenTrue(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	r := RunRecord{
		TimeNS: 1791720000000000000,
		Label:  "nested:child",
		Need:   map[string]spec.Range{"cpu": {Min: 2, Max: 2}},
		Grant:  map[string]int{"cpu": 2},
		Exit:   0,
	}
	if err := s.AppendRun(r); err != nil {
		t.Fatal(err)
	}
	nested := r
	nested.Nested = true
	if err := s.AppendRun(nested); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, filepath.Join(dir, LogDir, RunLog))
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2: %q", len(lines), lines)
	}
	var plain, marked map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &plain); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &marked); err != nil {
		t.Fatal(err)
	}
	if _, ok := plain["nested"]; ok {
		t.Errorf("a regular run's line has a nested key: %s", lines[0])
	}
	if v, ok := marked["nested"].(bool); !ok || !v {
		t.Errorf("a pass-through run's line = %s; want \"nested\":true as a JSON boolean", lines[1])
	}
	if !strings.HasSuffix(lines[1], `,"fail_open":"","nested":true}`) {
		t.Errorf("nested is not the last key, after fail_open: %s", lines[1])
	}
	if len(marked) != len(plain)+1 {
		t.Errorf("nested line has %d keys, regular line %d; want exactly one more", len(marked), len(plain))
	}
}

// A pass-through appends outside the section and never rotates: AppendRun leaves a log at or
// above 10 MiB where it is, and only Section.RotateLogIfNeeded renames it.
func TestAppendRunNeverRotates(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	r := RunRecord{TimeNS: 1791720000000000000, Label: "nested:child", Nested: true}
	if err := s.AppendRun(r); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, LogDir, RunLog)
	if err := os.Truncate(logPath, RotateRunLogAt+1); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRun(r); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(logPath); err != nil || fi.Size() <= RotateRunLogAt {
		t.Errorf("AppendRun rotated or truncated the log: %v, %v", fi, err)
	}
	if exists(filepath.Join(dir, LogDir, RunLogOld)) {
		t.Error("AppendRun created runs.1.jsonl")
	}
}
