//go:build unix

package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFormatAt(t *testing.T) {
	dir := newDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if n, present, err := ReadFormatAt(dir); err != nil || present || n != 0 {
		t.Fatalf("absent FORMAT: %d %v %v", n, present, err)
	}
	if names, _ := os.ReadDir(dir); len(names) != 0 {
		t.Fatalf("ReadFormatAt wrote %v", names)
	}
	if err := os.WriteFile(filepath.Join(dir, FormatFile), []byte("coreslot-protocol 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, present, err := ReadFormatAt(dir); err != nil || !present || n != 2 {
		t.Errorf("FORMAT 2: %d %v %v", n, present, err)
	}
	if err := os.WriteFile(filepath.Join(dir, FormatFile), []byte("coreslot-protocol x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ReadFormatAt(dir)
	wantStateError(t, err, "a malformed FORMAT")
}

// AppendRunAt appends a run-log line to a state directory that is not opened as a Store: it
// checks the policy of $S and log/ and creates nothing but log/ and its file.
func TestAppendRunAtCreatesNothing(t *testing.T) {
	dir := newDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FormatFile), []byte("coreslot-protocol 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendRunAt(dir, RunRecord{Label: "nested", Nested: true}); err != nil {
		t.Fatal(err)
	}
	names, _ := os.ReadDir(dir)
	var got []string
	for _, n := range names {
		got = append(got, n.Name())
	}
	if len(got) != 2 || got[0] != FormatFile || got[1] != LogDir {
		t.Errorf("the state directory holds %v, want only FORMAT and log", got)
	}
	if lines := readLines(t, filepath.Join(dir, LogDir, RunLog)); len(lines) != 1 {
		t.Errorf("run log %q, want one line", lines)
	}

	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	wantStateError(t, AppendRunAt(dir, RunRecord{Label: "x"}), "a group-writable state directory")
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	other := newDir(t)
	if err := os.MkdirAll(filepath.Join(other, "elsewhere"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "elsewhere"), filepath.Join(other, LogDir)); err != nil {
		t.Fatal(err)
	}
	wantStateError(t, AppendRunAt(other, RunRecord{Label: "x"}), "a log/ that is a symlink")
	missing := filepath.Join(t.TempDir(), "missing")
	if err := AppendRunAt(missing, RunRecord{Label: "x"}); err == nil || exists(missing) {
		t.Errorf("a missing state directory: err %v; want an error and nothing created", err)
	}
}
