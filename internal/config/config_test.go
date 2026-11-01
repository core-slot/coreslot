package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// m16 is the machine most tests here resolve against: 16 GiB.
var m16 = Machine{MemMiB: 16384}

func TestLoadMissingFileIsDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent", "config.toml"))
	if err != nil {
		t.Fatalf("Load of a missing file: %v", err)
	}
	if got := c.MemMiB(m16); got != 9830 {
		t.Errorf("MemMiB = %d, want 9830", got)
	}
	if !c.WindowsWarning {
		t.Error("WindowsWarning defaults to false, want true")
	}
}

// An empty file loads as the defaults, and a table the configuration does not know is ignored.
// Mutation: reject an unknown table, or size a resource besides mem.
func TestConfigMinimal(t *testing.T) {
	for _, body := range []string{"", "[nonesuch]\nkey = 1\n", "[resources]\nfuture = 3\n"} {
		c, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load(%q): %v", body, err)
		}
		if got := c.MemMiB(m16); got != 9830 || !c.WindowsWarning {
			t.Errorf("Load(%q): mem %d MiB, windows warning %v; want 9830 and the warning on", body, got, c.WindowsWarning)
		}
	}
}

func TestLoadFile(t *testing.T) {
	c, err := Load(writeConfig(t, `
[resources]
mem = "1G"

[platform]
windows_warning = false

[future-table]
anything = "is ignored"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.MemMiB(m16); got != 1024 {
		t.Errorf("MemMiB = %d, want 1024", got)
	}
	if c.WindowsWarning {
		t.Error("windows_warning = false not honoured")
	}
}

// With no file, mem is 60% of memory in MiB, rounded down. Mutation: a default of "50%", and
// this test reddens.
func TestDefaultSizes(t *testing.T) {
	c, msg := parse("")
	if msg != "" {
		t.Fatal(msg)
	}
	if got := c.MemMiB(m16); got != 9830 {
		t.Errorf("MemMiB = %d, want 9830", got)
	}
}

func TestMemSizes(t *testing.T) {
	for body, want := range map[string]int64{`"60%"`: 9830, "9": 9216, `"9.5G"`: 9728, `"768M"`: 768,
		`"1M"`: 1, `"100%"`: 16384, `"1000G"`: 1024000} {
		c, msg := parse("[resources]\nmem = " + body + "\n")
		if msg != "" {
			t.Errorf("mem = %s: %s", body, msg)
			continue
		}
		if got := c.MemMiB(m16); got != want {
			t.Errorf("mem = %s: %d MiB, want %d", body, got, want)
		}
	}
	for _, body := range []string{`"101%"`, `"-2"`, "true", "4.5", `"9GB"`, "0", `"0M"`} {
		if _, msg := parse("[resources]\nmem = " + body + "\n"); msg == "" {
			t.Errorf("mem = %s accepted", body)
		}
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	bodies := map[string]string{
		"malformed TOML":      "[resources\nmem = 1\n",
		"duplicate key":       "[resources]\nmem = 1\nmem = 2\n",
		"invalid size":        "[resources]\nmem = 0\n",
		"float size":          "[resources]\nmem = 2.5\n",
		"resources not table": "resources = 3\n",
		"warning not bool":    "[platform]\nwindows_warning = \"no\"\n",
	}
	for what, body := range bodies {
		_, err := Load(writeConfig(t, body))
		var ce *Error
		if !errors.As(err, &ce) {
			t.Errorf("%s: got %v, want a *config.Error", what, err)
		}
	}
}

// TestMemoryTable pins [memory] live: off unless the file turns it on. Mutation: defaulting
// LiveMem to true reddens the missing file.
func TestMemoryTable(t *testing.T) {
	for what, path := range map[string]string{
		"missing file":     filepath.Join(t.TempDir(), "absent", "config.toml"),
		"no [memory]":      writeConfig(t, "[resources]\nmem = \"1G\"\n"),
		"unknown key only": writeConfig(t, "[memory]\nfuture = 1\n"),
	} {
		c, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if c.LiveMem {
			t.Errorf("%s: LiveMem = true, want false", what)
		}
	}
	c, err := Load(writeConfig(t, "[memory]\nlive = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.LiveMem {
		t.Error("live = true not honoured")
	}
	c, err = Load(writeConfig(t, "[memory]\nlive = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.LiveMem {
		t.Error("live = false not honoured")
	}
	for body, want := range map[string]string{
		"memory = 3\n":              "[memory] is not a table",
		"[memory]\nlive = \"no\"\n": "[memory] live: want a boolean",
	} {
		_, err := Load(writeConfig(t, body))
		var ce *Error
		if !errors.As(err, &ce) || ce.Msg != want {
			t.Errorf("Load(%q) = %v, want a *config.Error %q", body, err, want)
		}
	}
}

// The configuration is read at every call (PROTOCOL.md section 5.2): a replacement that keeps
// the size and the mtime, by rewriting in place or by renaming another file over it, is seen at
// once.
func TestWatcherReadsEveryCall(t *testing.T) {
	path := writeConfig(t, "[resources]\nmem = \"1280M\"\n")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{Path: path}
	if c, err := w.Current(); err != nil || c.MemMiB(m16) != 1280 {
		t.Fatalf("first Current() = %v, %v", c.MemMiB(m16), err)
	}

	// Renamed over, same size, same mtime (cp -p to a temporary name, then mv).
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte("[resources]\nmem = \"1536M\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if c, err := w.Current(); err != nil || c.MemMiB(m16) != 1536 {
		t.Errorf("Current() after a rename keeping size and mtime: mem = %d, %v; want 1536", c.MemMiB(m16), err)
	}

	// Rewritten in place, same size, same mtime.
	if err := os.WriteFile(path, []byte("[resources]\nmem = \"1408M\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if c, err := w.Current(); err != nil || c.MemMiB(m16) != 1408 {
		t.Errorf("Current() after an in-place rewrite keeping size and mtime: mem = %d, %v; want 1408", c.MemMiB(m16), err)
	}

	// An invalid file is reported as a *config.Error.
	if err := os.WriteFile(path, []byte("[resources]\nmem = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ce *Error
	if _, err := w.Current(); !errors.As(err, &ce) {
		t.Errorf("Current() of an invalid file: %v, want a *config.Error", err)
	}

	// Removing the file returns to the defaults.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if c, err := w.Current(); err != nil || c.MemMiB(m16) != 9830 {
		t.Errorf("Current() after removal = %v, %v; want defaults", c.MemMiB(m16), err)
	}
}
