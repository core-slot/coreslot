//go:build unix

// Package forward is a run's forwarder: a relay between the run's command and Docker's socket that
// lives in an entry of its own, born with its lock held and ended by swapping its socket for a
// symlink to the upstream. It logs no byte of what it relays.
package forward

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/store"
)

// The names inside the forwarder's directory: an entry holds a lock and a socket, and is born under
// the prefix before it is renamed to its final name.
const (
	LockName  = "lock"
	SockName  = "docker.sock"
	NewPrefix = ".new-"
)

// swapName is the symlink a swap makes beside the socket before renaming it over the socket.
const swapName = SockName + ".swap"

// rename gives a born entry its final name; a test watches that moment.
var rename = os.Rename

// Birth makes the entry, the directory entry under its parent, with its lock held from before
// anyone can see its final name: it makes <parent>/.new-<base>/ (0700), creates lock in it and
// takes flock(LOCK_EX), binds the socket and chmods it 0600, then renames the directory to entry.
// The listener leaves the path alone when it closes, since a swap may have put a symlink there.
func Birth(entry string) (lock *os.File, ln net.Listener, err error) {
	if _, err := os.Lstat(entry); !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("forward: %s exists", entry)
	}
	tmp := filepath.Join(filepath.Dir(entry), NewPrefix+filepath.Base(entry))
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			if ln != nil {
				ln.Close()
			}
			if lock != nil {
				lock.Close()
			}
			lock, ln = nil, nil
			os.RemoveAll(tmp)
		}
	}()
	if err = os.Chmod(tmp, 0o700); err != nil { // the umask may have taken a bit of 0700
		return nil, nil, err
	}
	if lock, err = store.OpenFile(filepath.Join(tmp, LockName), unix.O_RDWR|unix.O_CREAT|unix.O_EXCL); err != nil {
		return lock, nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return lock, nil, fmt.Errorf("forward: lock %s: %w", tmp, err)
	}
	sock := filepath.Join(tmp, SockName)
	if ln, err = net.Listen("unix", sock); err != nil {
		return lock, ln, err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err = os.Chmod(sock, 0o600); err != nil {
		return lock, ln, err
	}
	if err = rename(tmp, entry); err != nil {
		return lock, ln, err
	}
	return lock, ln, nil
}

// Swap points the entry's address at upstream: a symlink made beside the socket and renamed over
// it, so that a connection made at any moment finds one or the other. An entry that is gone is
// recreated first (0700, a lock nobody holds), so that whatever still holds the address keeps
// reaching the upstream.
func Swap(entry, upstream string) error {
	fi, err := os.Lstat(entry)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(entry, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := os.Chmod(entry, 0o700); err != nil {
			return err
		}
		lock, err := store.OpenFile(filepath.Join(entry, LockName), unix.O_RDWR|unix.O_CREAT)
		if err != nil {
			return err
		}
		lock.Close()
	case err != nil:
		return err
	case !fi.IsDir():
		return fmt.Errorf("forward: %s is not a directory", entry)
	}
	tmp := filepath.Join(entry, swapName)
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Symlink(upstream, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(entry, SockName))
}
