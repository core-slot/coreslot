package config

import (
	"errors"
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
	if d, p, err := Locate(); err != nil || d != want || p != path {
		t.Errorf("Locate() = %q, %q, %v; want %q, %q", d, p, err, want, path)
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

	if d, p, err := Locate(); err != nil || d != base || p != path {
		t.Errorf("Locate() under the override = %q, %q, %v; want %q, %q", d, p, err, base, path)
	}

	t.Setenv("CORESLOT_STATE_DIR", "relative/dir")
	if _, _, err := Locate(); err == nil {
		t.Error("Locate() accepted a relative override")
	}
	if _, _, err := StateDir(); err == nil {
		t.Error("StateDir() accepted a relative override")
	}
	if _, err := Path(); err == nil {
		t.Error("Path() accepted a relative override")
	}
}

// StateDirFrom applies the CORESLOT_STATE_DIR rule of PROTOCOL.md section 4.1 to the variables
// getenv reads, and to nothing else: a non-empty value is the state directory, cleaned, and must
// be absolute; an empty one means the default under the home directory. Mutation: dropping the
// IsAbs check reddens "relative" and "dot"; returning the value uncleaned reddens "unclean";
// reading os.Getenv for the variable reddens "unset".
func TestStateDirFrom(t *testing.T) {
	t.Setenv(StateDirEnv, "/process/environment") // not getenv's: it must not count
	home, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(home, ".local", "state", "coreslot")
	if runtime.GOOS == "darwin" {
		def = filepath.Join(home, "Library", "Application Support", "coreslot")
	}
	for _, tc := range []struct {
		name, value string
		dir         string
		override    bool
		err         string
	}{
		{name: "unset", value: "", dir: def, override: false},
		{name: "absolute", value: "/srv/state", dir: "/srv/state", override: true},
		{name: "unclean", value: "/srv//state/./x/..", dir: "/srv/state", override: true},
		{name: "relative", value: "relative/dir", override: true, err: `CORESLOT_STATE_DIR="relative/dir" is not an absolute path`},
		{name: "dot", value: ".", override: true, err: `CORESLOT_STATE_DIR="." is not an absolute path`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string {
				if k == StateDirEnv {
					return tc.value
				}
				return ""
			}
			dir, override, err := StateDirFrom(getenv)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err || override != tc.override {
					t.Fatalf("StateDirFrom(%q) = %q, %v, %v; want the error %q", tc.value, dir, override, err, tc.err)
				}
				return
			}
			if err != nil || dir != tc.dir || override != tc.override {
				t.Fatalf("StateDirFrom(%q) = %q, %v, %v; want %q, %v", tc.value, dir, override, err, tc.dir, tc.override)
			}
		})
	}
}

// The state directory and the configuration file derive from one home directory lookup, since a
// lookup can start getent (Home); an override needs none. Mutation: look the home directory up
// again for the configuration file in locate and the default case counts 2.
func TestLocateLooksUpHomeOnce(t *testing.T) {
	const home = "/nonexistent/home"
	def := filepath.Join(home, ".local", "state", "coreslot")
	if runtime.GOOS == "darwin" {
		def = filepath.Join(home, "Library", "Application Support", "coreslot")
	}
	for _, tc := range []struct {
		name, value, dir, cfg string
		homeErr               error
		lookups               int
	}{
		{name: "default", dir: def, cfg: filepath.Join(home, ".config", "coreslot", "config.toml"), lookups: 1},
		{name: "override", value: "/srv/state", dir: "/srv/state", cfg: filepath.Join("/srv/state", "config.toml"), lookups: 0},
		{name: "lookup error", homeErr: errors.New("no record"), lookups: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups := 0
			lookup := func() (string, error) {
				lookups++
				if tc.homeErr != nil {
					return "", tc.homeErr
				}
				return home, nil
			}
			getenv := func(k string) string {
				if k == StateDirEnv {
					return tc.value
				}
				return ""
			}
			dir, _, cfg, err := locate(getenv, lookup)
			if !errors.Is(err, tc.homeErr) || dir != tc.dir || cfg != tc.cfg || lookups != tc.lookups {
				t.Errorf("locate = %q, %q, %v after %d lookups; want %q, %q, %v after %d",
					dir, cfg, err, lookups, tc.dir, tc.cfg, tc.homeErr, tc.lookups)
			}
		})
	}
}
