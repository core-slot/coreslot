package config

import (
	"errors"
	"maps"
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
	if got := c.Sizes(m14); !maps.Equal(got, map[string]int{"cpu": 19, "mem": 76}) {
		t.Errorf("Sizes = %v, want map[cpu:19 mem:76]", got)
	}
	if len(c.Resources) != 2 || len(c.Priorities) != 0 {
		t.Errorf("defaults = %+v, want cpu and mem only and no priorities", c)
	}
	if !c.WindowsWarning {
		t.Error("WindowsWarning defaults to false, want true")
	}
}

func TestLoadFile(t *testing.T) {
	path := writeConfig(t, `
[resources]
cpu = "-2"
pg-local = 2

[priorities]
"ci:" = 0
"interactive:" = 10

[platform]
windows_warning = false

[future-table]
anything = "is ignored"
`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Sizes(m14); !maps.Equal(got, map[string]int{"cpu": 12, "mem": 76, "pg-local": 2}) {
		t.Errorf("Sizes = %v", got)
	}
	if c.WindowsWarning {
		t.Error("windows_warning = false not honoured")
	}
	if p, ok := c.PriorityFor("interactive:shell"); !ok || p != 10 {
		t.Errorf("PriorityFor(interactive:shell) = %d, %v", p, ok)
	}

	// A file that declares other resources but not cpu still has cpu = 140%.
	c, err = Load(writeConfig(t, "[resources]\npg-local = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Sizes(m14); !maps.Equal(got, map[string]int{"cpu": 19, "mem": 76, "pg-local": 1}) {
		t.Errorf("Sizes without cpu = %v", got)
	}
}

func TestParseSize(t *testing.T) {
	const cpus = 14
	valid := []struct {
		in   any
		want int
	}{
		{int64(12), 12}, {12, 12}, {"100%", 14}, {"50%", 7}, {"1%", 1}, {"-2", 12}, {"-20", 1},
		{"-0", 14}, {int64(4096), 4096},
	}
	for _, tc := range valid {
		s, err := ParseSize(tc.in)
		if err != nil {
			t.Errorf("ParseSize(%#v): unexpected error %v", tc.in, err)
			continue
		}
		if got := s.Resolve(Machine{CPUs: cpus}); got != tc.want {
			t.Errorf("ParseSize(%#v).Resolve(%d) = %d, want %d", tc.in, cpus, got, tc.want)
		}
	}
	invalid := []any{
		int64(0), int64(4097), "0%", "401%", "abc", int64(-3), true, 1.5, "12", "", "%", "-", "+2",
		"50 %", "-2%", map[string]any{},
	}
	for _, in := range invalid {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%#v) accepted an invalid size", in)
		}
	}
	// A size computed above the protocol's maximum is the maximum.
	s, _ := ParseSize("100%")
	if got := s.Resolve(Machine{CPUs: 5000}); got != 4096 {
		t.Errorf(`"100%%".Resolve(5000) = %d, want 4096`, got)
	}
}

// m14 is the machine most tests here resolve against: 14 CPUs, 16 GiB.
var m14 = Machine{CPUs: 14, MemMiB: 16384}

// With no file, cpu is 140% of the CPUs and mem 60% of memory in 128 MiB tokens, rounded down.
// Mutation: a default cpu of "100%" instead of "140%", and this test reddens.
func TestDefaultSizes(t *testing.T) {
	c, msg := parse("")
	if msg != "" {
		t.Fatal(msg)
	}
	if got := c.Sizes(Machine{CPUs: 10, MemMiB: 16384}); !maps.Equal(got, map[string]int{"cpu": 14, "mem": 76}) {
		t.Errorf("Sizes = %v, want map[cpu:14 mem:76]", got)
	}
}

func TestCPUPercentUpTo400(t *testing.T) {
	s, err := ParseSize("400%")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Resolve(Machine{CPUs: 10}); got != 40 {
		t.Errorf(`"400%%" on 10 CPUs = %d, want 40`, got)
	}
	for _, bad := range []string{"401%", "0%"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) accepted", bad)
		}
	}
}

func TestMemSizes(t *testing.T) {
	m := Machine{CPUs: 8, MemMiB: 16384}
	for body, want := range map[string]int{`"60%"`: 76, "9": 72, `"9.5G"`: 76, `"768M"`: 6,
		`"1M"`: 1, `"100%"`: 128} {
		c, msg := parse("[resources]\nmem = " + body + "\n")
		if msg != "" {
			t.Errorf("mem = %s: %s", body, msg)
			continue
		}
		if got := c.Sizes(m)["mem"]; got != want {
			t.Errorf("mem = %s: %d tokens, want %d", body, got, want)
		}
	}
	for _, body := range []string{`"101%"`, `"-2"`, "true", "4.5", `"9GB"`} {
		if _, msg := parse("[resources]\nmem = " + body + "\n"); msg == "" {
			t.Errorf("mem = %s accepted", body)
		}
	}
	c, msg := parse("[resources]\nmem = 0\n")
	if msg != "" {
		t.Fatal(msg)
	}
	if got := c.Sizes(m); !maps.Equal(got, map[string]int{"cpu": 11}) {
		t.Errorf("mem = 0: Sizes = %v, want map[cpu:11]", got)
	}
}

func TestLoadRejectsInvalidResourceName(t *testing.T) {
	for _, name := range []string{"pg_local", "Pg", `"1cpu"`, `"cpu.x"`} {
		_, err := Load(writeConfig(t, "[resources]\n"+name+" = 2\n"))
		var ce *Error
		if !errors.As(err, &ce) {
			t.Errorf("resource %s: got %v, want a *config.Error", name, err)
		}
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	bodies := map[string]string{
		"malformed TOML":      "[resources\ncpu = 1\n",
		"duplicate key":       "[resources]\ncpu = 1\ncpu = 2\n",
		"invalid size":        "[resources]\ncpu = 0\n",
		"float size":          "[resources]\ncpu = 2.5\n",
		"resources not table": "resources = 3\n",
		"priority too high":   "[priorities]\n\"ci:\" = 1001\n",
		"priority not int":    "[priorities]\n\"ci:\" = \"high\"\n",
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

// TestMemoryTable pins [memory] live: on unless the file turns it off. Mutation: defaulting
// LiveMem to false reddens the missing file.
func TestMemoryTable(t *testing.T) {
	for what, path := range map[string]string{
		"missing file":     filepath.Join(t.TempDir(), "absent", "config.toml"),
		"no [memory]":      writeConfig(t, "[resources]\npg-local = 1\n"),
		"unknown key only": writeConfig(t, "[memory]\nfuture = 1\n"),
	} {
		c, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if !c.LiveMem {
			t.Errorf("%s: LiveMem = false, want true", what)
		}
	}
	c, err := Load(writeConfig(t, "[memory]\nlive = false\n"))
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

func TestPriorityLongestPrefix(t *testing.T) {
	c := Config{Priorities: map[string]int{"ci:": 0, "ci:urgent": 50, "interactive:": 10, "c": -5}}
	cases := []struct {
		label string
		want  int
		ok    bool
	}{
		{"ci:urgent-fix", 50, true},
		{"ci:o-r", 0, true},
		{"cargo", -5, true},
		{"interactive:x", 10, true},
		{"gate:unit", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := c.PriorityFor(tc.label)
		if got != tc.want || ok != tc.ok {
			t.Errorf("PriorityFor(%q) = %d, %v; want %d, %v", tc.label, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseOnlineCPUs(t *testing.T) {
	valid := map[string]int{"0-3,8-11\n": 8, "0\n": 1, "0-13\n": 14, "0,2,4-5": 4}
	for in, want := range valid {
		got, err := parseOnline(in)
		if err != nil || got != want {
			t.Errorf("parseOnline(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "\n", "a", "3-1", "0-", "-1", "0,,1", "0-3,", "1 2"} {
		if n, err := parseOnline(in); err == nil {
			t.Errorf("parseOnline(%q) = %d, want an error", in, n)
		}
	}
}

// The configuration is read at every call (PROTOCOL.md section 5.2): a replacement that keeps
// the size and the mtime, by rewriting in place or by renaming another file over it, is seen at
// once.
func TestWatcherReadsEveryCall(t *testing.T) {
	path := writeConfig(t, "[resources]\ncpu = 10\n")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{Path: path}
	if c, err := w.Current(); err != nil || c.Sizes(m14)["cpu"] != 10 {
		t.Fatalf("first Current() = %v, %v", c.Sizes(m14), err)
	}

	// Renamed over, same size, same mtime (cp -p to a temporary name, then mv).
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte("[resources]\ncpu = 12\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if c, err := w.Current(); err != nil || c.Sizes(m14)["cpu"] != 12 {
		t.Errorf("Current() after a rename keeping size and mtime: cpu = %d, %v; want 12", c.Sizes(m14)["cpu"], err)
	}

	// Rewritten in place, same size, same mtime.
	if err := os.WriteFile(path, []byte("[resources]\ncpu = 11\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if c, err := w.Current(); err != nil || c.Sizes(m14)["cpu"] != 11 {
		t.Errorf("Current() after an in-place rewrite keeping size and mtime: cpu = %d, %v; want 11", c.Sizes(m14)["cpu"], err)
	}

	// An invalid file is reported as a *config.Error.
	if err := os.WriteFile(path, []byte("[resources]\ncpu = 0\n"), 0o600); err != nil {
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
	if c, err := w.Current(); err != nil || !maps.Equal(c.Sizes(m14), map[string]int{"cpu": 19, "mem": 76}) {
		t.Errorf("Current() after removal = %v, %v; want defaults", c.Sizes(m14), err)
	}
}
