//go:build unix

package store

import (
	"bufio"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
		TimeNS:  1791720000000000000,
		Label:   "gate:unit\nsecond line \x1b[31mred\x1b[0m",
		WaitedS: 1.5,
		RanS:    12.25,
		Exit:    0,
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
	wantKeys := []string{"exit", "fail_open", "label", "ran_s", "time_ns", "waited_s"}
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
	}
	var second map[string]any
	_ = json.Unmarshal([]byte(lines[1]), &second)
	fo, _ := second["fail_open"].(string)
	if fo == "" || !strings.HasPrefix(long.FailOpen, fo) || !utf8.ValidString(fo) {
		t.Errorf("fail_open was not truncated to a valid prefix: %d bytes", len(fo))
	}
	tooLong := r
	tooLong.Label = strings.Repeat("x", MaxRunLine) // a label no request carries: nothing to shorten
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
	if err := sec.rotateLogIfNeeded(LogDir, RunLog, RunLogOld, RotateRunLogAt); err != nil {
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
	if err := sec.rotateLogIfNeeded(LogDir, RunLog, RunLogOld, RotateRunLogAt); err != nil {
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
	if err := os.WriteFile(filepath.Join(dir, FormatFile), []byte("coreslot-protocol 3\n"), 0o600); err != nil {
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

// PROTOCOL.md section 20: user_s and sys_s are encoded exactly when they are known, a measured
// zero included, and a line shortened to fit 4 KiB keeps them.
//
// Mutation: encode the two fields without omitempty (an unknown value is written as null), or as
// plain numbers with omitempty (a measured zero disappears).
func TestRunLogUsageKeys(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	user, sys := 1.25, 0.0
	r := RunRecord{TimeNS: 1791720000000000000, Label: "usage"}
	measured := r
	measured.UserS, measured.SysS = &user, &sys
	long := measured
	long.FailOpen = strings.Repeat("x", 5000)
	for _, rec := range []RunRecord{r, measured, long} {
		if err := s.AppendRun(rec); err != nil {
			t.Fatal(err)
		}
	}
	lines := readLines(t, filepath.Join(dir, LogDir, RunLog))
	if len(lines) != 3 {
		t.Fatalf("%d lines, want 3: %q", len(lines), lines)
	}
	for _, key := range []string{"user_s", "sys_s"} {
		if strings.Contains(lines[0], `"`+key+`"`) {
			t.Errorf("a line without usage has %s: %s", key, lines[0])
		}
	}
	const want = `"ran_s":0,"user_s":1.25,"sys_s":0,"exit":0`
	for i, line := range lines[1:] {
		if !strings.Contains(line, want) {
			t.Errorf("line %d = %s; want it to contain %s", i+1, line, want)
		}
	}
	if len(lines[2])+1 != MaxRunLine || !strings.Contains(lines[2], `"fail_open":"xxx`) {
		t.Errorf("the long line is %d bytes with its LF; want exactly %d, fail_open shortened",
			len(lines[2])+1, MaxRunLine)
	}
}

// The memory keys are written when known and left out when not, never as a measured zero.
// Mutation: drop omitempty from mem_peak_kib.
func TestRunRecordMemKeys(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	kib := int64(368640)
	r := RunRecord{TimeNS: 1791720000000000000, Label: "mem"}
	measured := r
	measured.MemPeakKiB = &kib
	for _, rec := range []RunRecord{r, measured} {
		if err := s.AppendRun(rec); err != nil {
			t.Fatal(err)
		}
	}
	lines := readLines(t, filepath.Join(dir, LogDir, RunLog))
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2: %q", len(lines), lines)
	}
	if strings.Contains(lines[0], `"mem_peak_kib"`) {
		t.Errorf("a line without memory has mem_peak_kib: %s", lines[0])
	}
	if want := `"mem_peak_kib":368640`; !strings.Contains(lines[1], want) {
		t.Errorf("line = %s; want it to contain %s", lines[1], want)
	}
}

// A record without the key reads as no container, the mark round-trips, and a false mark leaves
// the line byte for byte what a build without it wrote. Mutation: drop container's omitempty → the
// plain line gains the key.
func TestRunRecordContainerKey(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	plain := RunRecord{TimeNS: 1791720000000000000, Label: "x"}
	marked := plain
	marked.Container = true
	for _, r := range []RunRecord{plain, marked} {
		if err := s.AppendRun(r); err != nil {
			t.Fatal(err)
		}
	}
	lines := readLines(t, filepath.Join(dir, LogDir, RunLog))
	const old = `{"time_ns":1791720000000000000,"label":"x","waited_s":0,"ran_s":0,"exit":0,"fail_open":""`
	if len(lines) != 2 || lines[0] != old+"}" || lines[1] != old+`,"container":true}` {
		t.Fatalf("lines %q; want %s} and the same with the key", lines, old)
	}
	runs, skipped, err := s.ReadRuns()
	if err != nil || skipped != 0 || len(runs) != 2 || runs[0].Container || !runs[1].Container {
		t.Errorf("read back %+v (%d skipped, %v); want the mark on the second only", runs, skipped, err)
	}
}

// AppendRun appends outside the section and never rotates: it leaves a log at or
// above 10 MiB where it is, and only RotateLog renames it.
func TestAppendRunNeverRotates(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	r := RunRecord{TimeNS: 1791720000000000000, Label: "child"}
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

// ReadRuns reads the rotated generation first, then the current one.
// Mutation: read runs.jsonl before runs.1.jsonl, or skip runs.1.jsonl.
func TestReadRunsBothGenerations(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	if runs, skipped, err := s.ReadRuns(); err != nil || len(runs) != 0 || skipped != 0 {
		t.Fatalf("ReadRuns without a log = %v, %d, %v; want nothing", runs, skipped, err)
	}
	for _, label := range []string{"first", "second"} {
		if err := s.AppendRun(RunRecord{TimeNS: 1791720000000000000, Label: label}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(filepath.Join(dir, LogDir, RunLog), filepath.Join(dir, LogDir, RunLogOld)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRun(RunRecord{TimeNS: 1791720000000000000, Label: "third"}); err != nil {
		t.Fatal(err)
	}
	runs, skipped, err := s.ReadRuns()
	if err != nil || skipped != 0 {
		t.Fatalf("ReadRuns = %d skipped, %v", skipped, err)
	}
	var labels []string
	for _, r := range runs {
		labels = append(labels, r.Label)
	}
	if want := []string{"first", "second", "third"}; !slices.Equal(labels, want) {
		t.Errorf("labels %v, want %v", labels, want)
	}
}

// A line that does not decode is skipped and counted; an old line without the memory keys is
// kept, with those fields unknown.
// Mutation: return the decode error instead of skipping the line.
func TestReadRunsSkipsMalformed(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	data, err := os.ReadFile(filepath.Join("testdata", "runs-malformed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, LogDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, LogDir, RunLog), data, 0o600); err != nil {
		t.Fatal(err)
	}
	runs, skipped, err := s.ReadRuns()
	if err != nil || skipped != 2 || len(runs) != 1 {
		t.Fatalf("ReadRuns = %d runs, %d skipped, %v; want 1 run, 2 skipped", len(runs), skipped, err)
	}
	r := runs[0]
	if r.Label != "old:tests" || r.WaitedS != 0.5 || r.RanS != 10 {
		t.Errorf("old run %+v", r)
	}
	if r.MemPeakKiB != nil {
		t.Errorf("old run has memory fields it never recorded: %+v", r)
	}
}

// A record encodes exactly the run log's keys, and reads back field for field; stops is written
// only above 0. Mutation: drop a key from the encoder, or encode stops without omitempty.
func TestRunRecordKeys(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	user, sys, tick, peak := 3.5, 0.25, 0.04, int64(2831156)
	want := RunRecord{
		TimeNS: 1791720000000000000, Label: "fastapi:tests", WaitedS: 1.5, RanS: 12.25, Exit: 1,
		FailOpen: "timeout", MemPeakKiB: &peak, UserS: &user, SysS: &sys, TickShare: &tick, Stops: 2,
	}
	plain := RunRecord{TimeNS: want.TimeNS, Label: "plain"}
	for _, r := range []RunRecord{want, plain} {
		if err := s.AppendRun(r); err != nil {
			t.Fatal(err)
		}
	}
	lines := readLines(t, filepath.Join(dir, LogDir, RunLog))
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2: %q", len(lines), lines)
	}
	keys := func(line string) []string {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		return slices.Sorted(maps.Keys(m))
	}
	full := []string{"exit", "fail_open", "label", "mem_peak_kib", "ran_s", "stops", "sys_s",
		"tick_share", "time_ns", "user_s", "waited_s"}
	if got := keys(lines[0]); !slices.Equal(got, full) {
		t.Errorf("a full record's keys %v, want %v", got, full)
	}
	if got, want := keys(lines[1]), []string{"exit", "fail_open", "label", "ran_s", "time_ns", "waited_s"}; !slices.Equal(got, want) {
		t.Errorf("a plain record's keys %v, want %v", got, want)
	}
	runs, skipped, err := s.ReadRuns()
	if err != nil || skipped != 0 || len(runs) != 2 || !reflect.DeepEqual(runs[0], want) {
		t.Errorf("read back %+v (%d skipped, %v), want %+v", runs, skipped, err, want)
	}
}
