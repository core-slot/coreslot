//go:build unix

package forward

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/core-slot/coreslot/internal/store"
)

// DirName is the forwarder's directory in the home directory, or in the state directory when
// that is overridden. It is not part of the state directory's layout.
const DirName = ".coreslot-fw"

// MaxAddress is the longest address, in bytes, that a container can bind-mount: Docker Desktop's
// limit applies to the socket's real path.
const MaxAddress = 96

// Dir returns the forwarder's directory in base, the home directory (or the state directory under
// its override), by its real path, making it with mode 0700 when it is absent. It refuses a base
// that is not absolute or whose real path has a space, and a directory that fails the state
// directory's checks: anything but a real directory of the user's with no group or world write, or
// one on a network or shared filesystem. It never changes a directory that is already there.
func Dir(base string) (string, error) {
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("forward: %q is not an absolute path", base)
	}
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	if strings.Contains(real, " ") {
		return "", fmt.Errorf("forward: %q has a space", real)
	}
	dir := filepath.Join(real, DirName)
	switch err := os.Mkdir(dir, 0o700); {
	case err == nil:
		if err := os.Chmod(dir, 0o700); err != nil { // the umask may have taken a bit of 0700
			return "", err
		}
	case !errors.Is(err, fs.ErrExist):
		return "", err
	}
	if _, err := checkDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// checkDir applies the state directory's checks to the forwarder's directory dir; a missing one
// passes, and exists says which.
func checkDir(dir string) (exists bool, err error) {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err := store.CheckEntry(dir, true); err != nil {
		return true, err
	}
	return true, store.CheckFilesystem(dir)
}

// EntryName is the name of a run's entry: its pid, a dash, and its start in nanoseconds written in
// base 36, at most 18 bytes until 2120.
func EntryName(pid int, startNS int64) string {
	return strconv.Itoa(pid) + "-" + strconv.FormatInt(startNS, 36)
}

// ParseEntryName reports whether name is an entry's name: decimal digits, a dash, and lower-case
// base-36 digits.
func ParseEntryName(name string) bool {
	pid, start, ok := strings.Cut(name, "-")
	return ok && digits(pid, 10) && digits(start, 36)
}

// digits reports whether s is a non-empty run of lower-case digits of base.
func digits(s string, base int) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		v := 36
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'z':
			v = int(c-'a') + 10
		}
		if v >= base {
			return false
		}
	}
	return true
}

// Address is the socket of the entry name in the forwarder's directory dir, which Dir returned by
// its real path, so the address is the socket's real path. ok is false when it passes MaxAddress.
func Address(dir, name string) (addr string, ok bool) {
	addr = filepath.Join(dir, name, SockName)
	return addr, len(addr) <= MaxAddress
}
