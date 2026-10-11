package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestHomeIgnoresHOME(t *testing.T) {
	t.Setenv("HOME", "/nonexistent")
	t.Setenv("XDG_STATE_HOME", "/nonexistent/state")
	t.Setenv("XDG_CONFIG_HOME", "/nonexistent/config")
	home, err := Home()
	if err != nil {
		t.Fatalf("Home(): %v", err)
	}
	if home == "" || home == "/nonexistent" {
		t.Fatalf("Home() = %q: it followed $HOME", home)
	}
	t.Setenv("CORESLOT_STATE_DIR", "")
	dir, override, err := StateDir()
	if err != nil || override {
		t.Fatalf("StateDir() = %q, %v, %v", dir, override, err)
	}
	want := filepath.Join(home, ".local", "state", "coreslot")
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Application Support", "coreslot")
	}
	if dir != want {
		t.Errorf("StateDir() = %q, want %q", dir, want)
	}
	path, err := Path()
	if err != nil || path != filepath.Join(home, ".config", "coreslot", "config.toml") {
		t.Errorf("Path() = %q, %v; want %q", path, err, filepath.Join(home, ".config", "coreslot", "config.toml"))
	}
}

func TestStateDirOverride(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CORESLOT_STATE_DIR", base)
	dir, override, err := StateDir()
	if err != nil || !override || dir != base {
		t.Fatalf("StateDir() = %q, %v, %v; want %q, true", dir, override, err, base)
	}
	// The override moves the configuration with the state.
	path, err := Path()
	if err != nil || path != filepath.Join(base, "config.toml") {
		t.Errorf("Path() under the override = %q, %v; want %q", path, err, filepath.Join(base, "config.toml"))
	}

	t.Setenv("CORESLOT_STATE_DIR", "relative/dir")
	if _, _, err := StateDir(); err == nil {
		t.Error("StateDir() accepted a relative override")
	}
	if _, err := Path(); err == nil {
		t.Error("Path() accepted a relative override")
	}
}
