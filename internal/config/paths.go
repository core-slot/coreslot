package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/testhooks"
)

// StateDirEnv is the only override of the state directory (PROTOCOL.md section 4.1).
const StateDirEnv = "CORESLOT_STATE_DIR"

// Home returns the user's home directory from the password database (getpwuid(getuid())),
// never from $HOME or XDG variables. On Windows, which has no password database, it is the
// current user's profile directory.
//
// Without cgo, os/user on Linux and the other non-darwin Unixes reads /etc/passwd alone and
// never consults NSS, so an account served by LDAP, SSSD, systemd-homed or winbind is unknown to
// it. When the lookup finds no record, Home asks NSS through getent, which resolves the uid the
// way getpwuid does (see homeOf). darwin's os/user calls libc even without cgo.
func Home() (string, error) {
	if runtime.GOOS == "windows" {
		u, err := user.Current()
		if err != nil {
			return "", fmt.Errorf("home directory from the password database: %w", err)
		}
		if u.HomeDir == "" {
			return "", fmt.Errorf("the password database has no home directory for uid %d", os.Getuid())
		}
		return u.HomeDir, nil
	}
	getent := getentPaths
	if p := testhooks.Getent(); p != "" {
		getent = []string{p}
	}
	return homeOf(os.Getuid(), user.LookupId, getent, getentTimeout)
}

// getentPaths are the fixed places getent is looked for, in order: glibc's on merged-/usr and
// split-/usr systems, and NixOS's system profile, which has no /usr/bin/getent. They are never
// searched through $PATH: the protocol takes the home directory from the password database, not
// the environment, so that a shell, a cron job and a service of one user find one budget; a
// $PATH search would let a directory early in one process's $PATH (a user's ~/bin) choose the
// program, and cron's short $PATH would find none. Each is a root-owned system location.
var getentPaths = []string{"/usr/bin/getent", "/bin/getent", "/run/current-system/sw/bin/getent"}

// getentTimeout bounds the NSS fallback. An NSS backend can hang on an unreachable directory
// server (an LDAP bind waits out its own timeouts); a lookup that answers at all answers well
// within it, and a run that cannot learn its home directory fails open rather than hang.
const getentTimeout = 5 * time.Second

// getentWaitDelay bounds the wait for getent's output after getent has exited or been killed,
// in case a process it started still holds the pipe.
const getentWaitDelay = 500 * time.Millisecond

// homeOf returns uid's home directory: lookupID's answer, or, when lookupID has no record for
// uid, the answer of the first of the getent paths that exists. The fallback runs only for a
// missing record (user.UnknownUserIdError) or a missing /etc/passwd (fs.ErrNotExist): those are
// the cases where a reader of /etc/passwd alone knows less than NSS. Every other error (an
// unreadable /etc/passwd, or an error of getpwuid_r itself in a cgo build) reports a broken
// system rather than a record kept elsewhere, and is returned as it is. getent is not started
// when the lookup succeeds. When the fallback fails too, the lookup's own error is returned,
// wrapped with the fallback's.
func homeOf(uid int, lookupID func(string) (*user.User, error), getent []string, timeout time.Duration) (string, error) {
	u, err := lookupID(strconv.Itoa(uid))
	if err != nil {
		var unknown user.UnknownUserIdError
		if !errors.As(err, &unknown) && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("home directory from the password database: %w", err)
		}
		home, nssErr := getentHome(getent, uid, timeout)
		if nssErr != nil {
			return "", fmt.Errorf("home directory from the password database: %w (the NSS fallback failed too: %v)", err, nssErr)
		}
		return home, nil
	}
	if u.HomeDir == "" {
		return "", fmt.Errorf("the password database has no home directory for uid %d", uid)
	}
	return u.HomeDir, nil
}

// getentHome runs `getent passwd <uid>` with the first of paths that is a regular file, for at
// most timeout, and returns the home directory (the sixth field) of its answer. The answer must
// be exactly one newline-terminated line of seven colon-separated fields whose third is uid and
// whose sixth is an absolute path; anything else is an error, never a guess.
//
// getent inherits the environment, as getpwuid in the user's other clients does: a variable that
// changes NSS (an LD_PRELOAD of nss_wrapper) changes their answer the same way. Its standard
// input is /dev/null and its standard error is captured, never the caller's. At the deadline
// os/exec kills getent: a child of this call, not a process the caller owns, and nothing
// process-wide changes.
func getentHome(paths []string, uid int, timeout time.Duration) (string, error) {
	path := ""
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			path = p
			break
		}
	}
	if path == "" {
		return "", fmt.Errorf("no getent at %s", strings.Join(paths, ", "))
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "passwd", strconv.Itoa(uid))
	cmd.WaitDelay = getentWaitDelay
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s passwd %d did not answer within %v", path, uid, timeout)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 { // getent's status for a key it did not find
		return "", fmt.Errorf("%s has no entry for uid %d", path, uid)
	}
	if err != nil {
		return "", fmt.Errorf("%s passwd %d: %v", path, uid, err)
	}
	line, ok := strings.CutSuffix(string(out), "\n")
	if !ok || line == "" || strings.Contains(line, "\n") {
		return "", fmt.Errorf("%s passwd %d printed %d bytes, not exactly one line", path, uid, len(out))
	}
	fields := strings.Split(line, ":")
	if len(fields) != 7 {
		return "", fmt.Errorf("%s passwd %d printed %d colon-separated fields, not 7", path, uid, len(fields))
	}
	if fields[2] != strconv.Itoa(uid) {
		return "", fmt.Errorf("%s passwd %d printed the entry of uid %q, not %d", path, uid, fields[2], uid)
	}
	home := fields[5]
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("%s passwd %d printed the home directory %q, which is not an absolute path", path, uid, home)
	}
	return home, nil
}

// StateDir returns the state directory and whether it comes from CORESLOT_STATE_DIR. The
// override must be an absolute path.
func StateDir() (dir string, override bool, err error) {
	return StateDirFrom(os.Getenv)
}

// StateDirFrom is StateDir for the environment getenv reads: the one rule of PROTOCOL.md section
// 4.1 for this process and for a context it is handed, such as an inherited grant's. Only the
// override comes from getenv; the default comes from the password database, never the
// environment.
func StateDirFrom(getenv func(string) string) (dir string, override bool, err error) {
	dir, override, _, err = locate(getenv, Home)
	return dir, override, err
}

// Path returns the configuration file: <statedir>/config.toml under the state-directory
// override, so a test budget never pairs with the user's real configuration; otherwise
// <home>/.config/coreslot/config.toml.
func Path() (string, error) {
	_, path, err := Locate()
	return path, err
}

// Locate returns the state directory and its configuration file together, the pair every opener
// of the budget uses: CORESLOT_STATE_DIR with <statedir>/config.toml, or the defaults of
// PROTOCOL.md section 4.1 with <home>/.config/coreslot/config.toml.
func Locate() (dir, cfgPath string, err error) {
	dir, _, cfgPath, err = locate(os.Getenv, Home)
	return dir, cfgPath, err
}

// locate is the rule of PROTOCOL.md section 4.1 behind StateDirFrom and Locate: the state
// directory, whether it is the override, and its configuration file. It calls home at most once,
// since a lookup can start getent (Home) and the default state directory and configuration file
// both derive from the one home directory.
func locate(getenv func(string) string, home func() (string, error)) (dir string, override bool, cfgPath string, err error) {
	if v := getenv(StateDirEnv); v != "" {
		if !filepath.IsAbs(v) {
			return "", true, "", fmt.Errorf("%s=%q is not an absolute path", StateDirEnv, v)
		}
		dir = filepath.Clean(v)
		return dir, true, filepath.Join(dir, "config.toml"), nil
	}
	h, err := home()
	if err != nil {
		return "", false, "", err
	}
	dir = filepath.Join(h, ".local", "state", "coreslot")
	if runtime.GOOS == "darwin" {
		dir = filepath.Join(h, "Library", "Application Support", "coreslot")
	}
	return dir, false, filepath.Join(h, ".config", "coreslot", "config.toml"), nil
}
