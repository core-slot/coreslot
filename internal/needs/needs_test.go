//go:build unix

package needs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, path string) File {
	t.Helper()
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// An entry holds a mem pin, as a string or a TOML integer of GiB, and the no_restart mark; a key
// it does not know is ignored, and an empty label finds nothing. Mutation: read only strings →
// entry "b" red; reject an unknown key → entry "a" red.
func TestNeedsEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "needs.toml")
	write(t, path, `
[needs."a"]
mem = "2G"
no_restart = true
colour = "red"
[needs."b"]
mem = 3
[needs.""]
mem = 1
`)
	f := load(t, path)
	for label, want := range map[string]Entry{
		"a": {MemMiB: 2048, NoRestart: true},
		"b": {MemMiB: 3072},
	} {
		if got, ok, err := f.Lookup(label); err != nil || !ok || got != want {
			t.Errorf("Lookup(%q) = %+v, %v, %v; want %+v", label, got, ok, err, want)
		}
	}
	for _, label := range []string{"", "c"} {
		if _, ok, err := f.Lookup(label); ok || err != nil {
			t.Errorf("Lookup(%q) = %v, %v; want nothing", label, ok, err)
		}
	}
}

// A bad value is an error naming the label and the key. Mutation: drop the key from the message
// → red.
func TestLookupBadValue(t *testing.T) {
	for _, key := range []string{`mem = "x"`, `mem = "0"`, `no_restart = "no"`} {
		path := filepath.Join(t.TempDir(), "needs.toml")
		write(t, path, "[needs.\"a\"]\n"+key+"\n")
		_, ok, err := load(t, path).Lookup("a")
		if ok || err == nil || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), strings.Fields(key)[0]) {
			t.Fatalf("%s: Lookup = %v, %v; want an error naming \"a\" and the key", key, ok, err)
		}
	}
}

func TestLoadMissing(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "needs.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.Lookup("a"); ok || err != nil {
		t.Fatalf("Lookup in a missing file = %v, %v", ok, err)
	}
}
