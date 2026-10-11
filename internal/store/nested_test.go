//go:build unix

package store

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TokenFiles lists the token files of a state directory without opening it as a Store: it
// creates nothing, skips names that are not tokens, and refuses what it cannot check.
func TestTokenFiles(t *testing.T) {
	dir := newDir(t)
	if err := os.MkdirAll(filepath.Join(dir, ResDir, "cpu"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ResDir, "Not-A-Name"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{tokenPath(dir, "cpu", 0), tokenPath(dir, "cpu", 13),
		filepath.Join(dir, ResDir, "cpu", "007"), filepath.Join(dir, ResDir, "Not-A-Name", "0")} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := TokenFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("TokenFiles = %+v, want cpu/0 and cpu/13 only", files)
	}
	for _, f := range files {
		var st syscall.Stat_t
		if err := syscall.Stat(f.Path, &st); err != nil {
			t.Fatal(err)
		}
		if f.Resource != "cpu" || f.Path != tokenPath(dir, "cpu", f.Index) || f.Dev != uint64(st.Dev) || f.Ino != uint64(st.Ino) {
			t.Errorf("token %+v does not describe its file", f)
		}
	}
	for _, name := range []string{AdmitLock, FormatFile, QueueDir, GenFile} {
		if exists(filepath.Join(dir, name)) {
			t.Errorf("TokenFiles created %s", name)
		}
	}

	if err := os.Symlink(tokenPath(dir, "cpu", 0), tokenPath(dir, "cpu", 1)); err != nil {
		t.Fatal(err)
	}
	_, err = TokenFiles(dir)
	wantStateError(t, err, "a token file that is a symlink")

	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := TokenFiles(missing); err == nil {
		t.Error("a missing state directory was listed")
	}
	if exists(missing) {
		t.Error("TokenFiles created the missing state directory")
	}
	noRes := newDir(t)
	if err := os.Mkdir(noRes, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := TokenFiles(noRes); err == nil {
		t.Error("a state directory without res/ was listed")
	}
	if exists(filepath.Join(noRes, ResDir)) {
		t.Error("TokenFiles created res/")
	}
}

// ReadFormatAt reads FORMAT like Store.ReadFormat without opening the directory as a Store.
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
