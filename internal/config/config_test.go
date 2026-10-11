package config

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
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
	if got := c.Sizes(14); !maps.Equal(got, map[string]int{"cpu": 14}) {
		t.Errorf("Sizes(14) = %v, want map[cpu:14]", got)
	}
	if len(c.Resources) != 1 || len(c.Priorities) != 0 {
		t.Errorf("defaults = %+v, want cpu only and no priorities", c)
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
	if got := c.Sizes(14); !maps.Equal(got, map[string]int{"cpu": 12, "pg-local": 2}) {
		t.Errorf("Sizes(14) = %v", got)
	}
	if c.WindowsWarning {
		t.Error("windows_warning = false not honoured")
	}
	if p, ok := c.PriorityFor("interactive:shell"); !ok || p != 10 {
		t.Errorf("PriorityFor(interactive:shell) = %d, %v", p, ok)
	}

	// A file that declares other resources but not cpu still has cpu = 100%.
	c, err = Load(writeConfig(t, "[resources]\npg-local = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Sizes(14); !maps.Equal(got, map[string]int{"cpu": 14, "pg-local": 1}) {
		t.Errorf("Sizes(14) without cpu = %v", got)
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
		if got := s.Resolve(cpus); got != tc.want {
			t.Errorf("ParseSize(%#v).Resolve(%d) = %d, want %d", tc.in, cpus, got, tc.want)
		}
	}
	invalid := []any{
		int64(0), int64(4097), "0%", "101%", "abc", int64(-3), true, 1.5, "12", "", "%", "-", "+2",
		"50 %", "-2%", map[string]any{},
	}
	for _, in := range invalid {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%#v) accepted an invalid size", in)
		}
	}
	// A size computed above the protocol's maximum is the maximum.
	s, _ := ParseSize("100%")
	if got := s.Resolve(5000); got != 4096 {
		t.Errorf(`"100%%".Resolve(5000) = %d, want 4096`, got)
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

func TestMachineCPUs(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("machine CPU count is implemented on macOS and Linux")
	}
	n, err := MachineCPUs()
	if err != nil || n < 1 {
		t.Fatalf("MachineCPUs() = %d, %v", n, err)
	}
}

func TestWatcherReloadsOnMtime(t *testing.T) {
	path := writeConfig(t, "[resources]\ncpu = 3\n")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{Path: path}
	c, err := w.Current()
	if err != nil || c.Sizes(14)["cpu"] != 3 {
		t.Fatalf("first Current() = %v, %v", c.Sizes(14), err)
	}

	// Same size, same mtime: the cached configuration is kept.
	if err := os.WriteFile(path, []byte("[resources]\ncpu = 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if c, _ := w.Current(); c.Sizes(14)["cpu"] != 3 {
		t.Errorf("Current() reloaded without an mtime change: cpu = %d", c.Sizes(14)["cpu"])
	}

	// A new mtime: reloaded.
	newer := old.Add(time.Minute)
	if err := os.Chtimes(path, newer, newer); err != nil {
		t.Fatal(err)
	}
	if c, err := w.Current(); err != nil || c.Sizes(14)["cpu"] != 4 {
		t.Errorf("Current() after an mtime change: cpu = %d, %v; want 4", c.Sizes(14)["cpu"], err)
	}

	// An invalid file is reported as a *config.Error.
	if err := os.WriteFile(path, []byte("[resources]\ncpu = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newer = newer.Add(time.Minute)
	_ = os.Chtimes(path, newer, newer)
	var ce *Error
	if _, err := w.Current(); !errors.As(err, &ce) {
		t.Errorf("Current() of an invalid file: %v, want a *config.Error", err)
	}

	// Removing the file returns to the defaults.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if c, err := w.Current(); err != nil || !maps.Equal(c.Sizes(14), map[string]int{"cpu": 14}) {
		t.Errorf("Current() after removal = %v, %v; want defaults", c.Sizes(14), err)
	}
}
