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

func TestLoadMissingFileIsDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent", "config.toml"))
	if err != nil {
		t.Fatalf("Load of a missing file: %v", err)
	}
	if c.ReserveMiB != DefaultReserveMiB {
		t.Errorf("ReserveMiB = %d, want %d", c.ReserveMiB, DefaultReserveMiB)
	}
	if !c.WindowsWarning {
		t.Error("WindowsWarning defaults to false, want true")
	}
}

// An empty file loads as the defaults, and a table or key the configuration does not know is
// ignored. Mutation: reject an unknown table.
func TestConfigMinimal(t *testing.T) {
	for _, body := range []string{"", "[nonesuch]\nkey = 1\n", "[memory]\nfuture = 3\n"} {
		c, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load(%q): %v", body, err)
		}
		if c.ReserveMiB != DefaultReserveMiB || !c.WindowsWarning {
			t.Errorf("Load(%q): reserve %d MiB, windows warning %v; want %d and the warning on", body, c.ReserveMiB, c.WindowsWarning, DefaultReserveMiB)
		}
	}
}

func TestLoadFile(t *testing.T) {
	c, err := Load(writeConfig(t, `
[memory]
reserve = "2G"

[platform]
windows_warning = false

[future-table]
anything = "is ignored"
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.ReserveMiB != 2048 {
		t.Errorf("ReserveMiB = %d, want 2048", c.ReserveMiB)
	}
	if c.WindowsWarning {
		t.Error("windows_warning = false not honoured")
	}
}

// reserve is a size in the rules of spec.ParseBytes or an integer of GiB; 0 is allowed.
// Mutation: reject 0, or read an integer as MiB.
func TestReserveSizes(t *testing.T) {
	for body, want := range map[string]int64{`"768M"`: 768, "2": 2048, `"1.5G"`: 1536, "0": 0, `"0M"`: 0} {
		c, msg := parse("[memory]\nreserve = " + body + "\n")
		if msg != "" {
			t.Errorf("reserve = %s: %s", body, msg)
			continue
		}
		if c.ReserveMiB != want {
			t.Errorf("reserve = %s: %d MiB, want %d", body, c.ReserveMiB, want)
		}
	}
	for _, body := range []string{`"-2"`, "true", "4.5", `"9GB"`, `"lots"`} {
		if _, msg := parse("[memory]\nreserve = " + body + "\n"); msg == "" {
			t.Errorf("reserve = %s accepted", body)
		}
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	bodies := map[string]string{
		"malformed TOML":   "[memory\nreserve = 1\n",
		"duplicate key":    "[memory]\nreserve = 1\nreserve = 2\n",
		"invalid size":     "[memory]\nreserve = \"lots\"\n",
		"float size":       "[memory]\nreserve = 2.5\n",
		"memory not table": "memory = 3\n",
		"warning not bool": "[platform]\nwindows_warning = \"no\"\n",
	}
	for what, body := range bodies {
		_, err := Load(writeConfig(t, body))
		var ce *Error
		if !errors.As(err, &ce) {
			t.Errorf("%s: got %v, want a *config.Error", what, err)
		}
	}
}

// TestMemoryTable pins the shapes of [memory]: a table, with reserve a size.
func TestMemoryTable(t *testing.T) {
	for body, want := range map[string]string{
		"memory = 3\n":                 "[memory] is not a table",
		"[memory]\nreserve = \"no\"\n": `[memory] reserve: "no" is not a size: want a number of GiB, such as 3 or 2.5G, or of MiB, such as 768M`,
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
	path := writeConfig(t, "[memory]\nreserve = \"1280M\"\n")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{Path: path}
	if c, err := w.Current(); err != nil || c.ReserveMiB != 1280 {
		t.Fatalf("first Current() = %v, %v", c.ReserveMiB, err)
	}

	// Renamed over, same size, same mtime (cp -p to a temporary name, then mv).
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte("[memory]\nreserve = \"1536M\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if c, err := w.Current(); err != nil || c.ReserveMiB != 1536 {
		t.Errorf("Current() after a rename keeping size and mtime: reserve = %d, %v; want 1536", c.ReserveMiB, err)
	}

	// Rewritten in place, same size, same mtime.
	if err := os.WriteFile(path, []byte("[memory]\nreserve = \"1408M\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if c, err := w.Current(); err != nil || c.ReserveMiB != 1408 {
		t.Errorf("Current() after an in-place rewrite keeping size and mtime: reserve = %d, %v; want 1408", c.ReserveMiB, err)
	}

	// An invalid file is reported as a *config.Error.
	if err := os.WriteFile(path, []byte("[memory]\nreserve = \"lots\"\n"), 0o600); err != nil {
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
	if c, err := w.Current(); err != nil || c.ReserveMiB != DefaultReserveMiB {
		t.Errorf("Current() after removal = %v, %v; want defaults", c.ReserveMiB, err)
	}
}
