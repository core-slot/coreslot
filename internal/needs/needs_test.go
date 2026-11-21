//go:build unix

package needs

import (
	"os"
	"path/filepath"
	"strconv"
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

// docker_forward is a boolean, read as a pointer: absent is nil, so that false and absent stay
// apart. Mutation: read it into a plain bool → the absent entry's nil, red.
func TestDockerForwardBoolean(t *testing.T) {
	path := filepath.Join(t.TempDir(), "needs.toml")
	write(t, path, "[needs.\"on\"]\ndocker_forward = true\n[needs.\"off\"]\ndocker_forward = false\n[needs.\"absent\"]\nmem = \"1G\"\n")
	f := load(t, path)
	for label, want := range map[string]*bool{"on": ptr(true), "off": ptr(false), "absent": nil} {
		e, ok, err := f.Lookup(label)
		if err != nil || !ok {
			t.Fatalf("Lookup(%q) = %v, %v", label, ok, err)
		}
		if (e.DockerForward == nil) != (want == nil) || want != nil && *e.DockerForward != *want {
			t.Errorf("Lookup(%q).DockerForward = %v, want %v", label, show(e.DockerForward), show(want))
		}
	}
}

// A docker_forward that is not a boolean makes the whole entry unreadable, as a bad mem does: a
// mistyped switch must not turn into "on". Mutation: ignore a non-boolean → ok, red.
func TestDockerForwardBadMakesEntryUnreadable(t *testing.T) {
	for _, v := range []string{`"yes"`, `1`, `"true"`} {
		path := filepath.Join(t.TempDir(), "needs.toml")
		write(t, path, "[needs.\"a\"]\nmem = \"1G\"\ndocker_forward = "+v+"\n")
		e, ok, err := load(t, path).Lookup("a")
		if ok || err == nil || !strings.Contains(err.Error(), "docker_forward") || e.MemMiB != 0 {
			t.Errorf("docker_forward = %s: Lookup = %+v, %v, %v; want an error naming docker_forward and no entry", v, e, ok, err)
		}
	}
}

func ptr(b bool) *bool { return &b }

func show(b *bool) string {
	if b == nil {
		return "nil"
	}
	return strconv.FormatBool(*b)
}

// docker_mem is a size above 0, as mem is: the label's Docker need in MiB. "0" or a value that is
// not a size makes the whole entry unreadable, as a bad docker_forward does, so that the run gets
// neither a forwarder nor a gate. Mutation: accept 0 → the "0" row is read, red.
func TestNeedsDockerMem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "needs.toml")
	write(t, path, "[needs.\"a\"]\ndocker_mem = \"2G\"\n[needs.\"b\"]\ndocker_mem = 3\n")
	f := load(t, path)
	for label, want := range map[string]int64{"a": 2048, "b": 3072} {
		if e, ok, err := f.Lookup(label); err != nil || !ok || e.DockerMemMiB != want || e.MemMiB != 0 {
			t.Errorf("Lookup(%q) = %+v, %v, %v; want DockerMemMiB %d", label, e, ok, err, want)
		}
	}
	for _, v := range []string{`"0"`, `0`, `"lots"`} {
		write(t, path, "[needs.\"a\"]\nmem = \"1G\"\ndocker_mem = "+v+"\n")
		e, ok, err := load(t, path).Lookup("a")
		if ok || err == nil || !strings.Contains(err.Error(), "docker_mem") || e.MemMiB != 0 {
			t.Errorf("docker_mem = %s: Lookup = %+v, %v, %v; want an error naming docker_mem and no entry", v, e, ok, err)
		}
	}
}
