package config

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
)

// StateDirEnv is the only override of the state directory (PROTOCOL.md section 4.1).
const StateDirEnv = "CORESLOT_STATE_DIR"

// Home returns the user's home directory from the password database (getpwuid(getuid())),
// never from $HOME or XDG variables. On Windows, which has no password database, it is the
// current user's profile directory.
func Home() (string, error) {
	var u *user.User
	var err error
	if runtime.GOOS == "windows" {
		u, err = user.Current()
	} else {
		u, err = user.LookupId(strconv.Itoa(os.Getuid()))
	}
	if err != nil {
		return "", fmt.Errorf("home directory from the password database: %w", err)
	}
	if u.HomeDir == "" {
		return "", fmt.Errorf("the password database has no home directory for uid %d", os.Getuid())
	}
	return u.HomeDir, nil
}

// StateDir returns the state directory and whether it comes from CORESLOT_STATE_DIR. The
// override must be an absolute path.
func StateDir() (dir string, override bool, err error) {
	if v := os.Getenv(StateDirEnv); v != "" {
		if !filepath.IsAbs(v) {
			return "", true, fmt.Errorf("%s=%q is not an absolute path", StateDirEnv, v)
		}
		return filepath.Clean(v), true, nil
	}
	home, err := Home()
	if err != nil {
		return "", false, err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "coreslot"), false, nil
	}
	return filepath.Join(home, ".local", "state", "coreslot"), false, nil
}

// Path returns the configuration file: <statedir>/config.toml under the state-directory
// override, so a test budget never pairs with the user's real configuration; otherwise
// <home>/.config/coreslot/config.toml.
func Path() (string, error) {
	dir, override, err := StateDir()
	if err != nil {
		return "", err
	}
	if override {
		return filepath.Join(dir, "config.toml"), nil
	}
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "coreslot", "config.toml"), nil
}
