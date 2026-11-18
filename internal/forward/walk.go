//go:build unix

package forward

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/store"
)

const (
	stampName = ".walked"           // in the forwarder's directory: its mtime is the last walk's start
	walkEvery = time.Hour           // the machine's walks are at least this far apart
	swapKeep  = 30 * 24 * time.Hour // a swapped socket's life, by its own time
	maxNames  = 65536               // names one pass reads
)

// Walk removes the entries of the forwarder's directory dir that nothing serves, at most once an
// hour for the machine: an entry or a birth (.new-) whose lock it can take, if it is a birth or
// its socket is a socket (its forwarder died unswapped) or a symlink older than 30 days by its
// own time. Anything else is left alone: a held lock, another type at docker.sock, a name that
// is not an entry's, an entry that is not a directory of the user's. It reads at most 65 536 names
// and processes those; the next due walk takes the rest. It never connects to a socket. A run
// may ignore its error: removing entries is a convenience.
func Walk(dir string, now time.Time) (removed int, err error) {
	if _, err := checkDir(dir); err != nil {
		return 0, err
	}
	if due, err := walkDue(dir, now); err != nil || !due {
		return 0, err
	}
	return eachFree(dir, func(path string, birth bool) bool {
		if birth {
			return true
		}
		fi, err := os.Lstat(filepath.Join(path, SockName))
		switch {
		case err != nil:
			return false
		case fi.Mode().Type() == fs.ModeSocket:
			return true
		case fi.Mode().Type() == fs.ModeSymlink:
			return now.Sub(fi.ModTime()) > swapKeep
		}
		return false
	})
}

// walkDue reports whether an hour has passed since the last walk, by the stamp's mtime, and if so
// sets that mtime to now. A missing stamp is due; so is one from the future (a clock set back).
func walkDue(dir string, now time.Time) (bool, error) {
	path := filepath.Join(dir, stampName)
	f, err := store.OpenFile(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
	fresh := err == nil
	if errors.Is(err, fs.ErrExist) {
		f, err = store.OpenFile(path, unix.O_RDWR)
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	if !fresh {
		fi, err := f.Stat()
		if err != nil {
			return false, err
		}
		if d := now.Sub(fi.ModTime()); d >= 0 && d < walkEvery {
			return false, nil
		}
	}
	tv := unix.NsecToTimeval(now.UnixNano())
	return true, unix.Futimes(int(f.Fd()), []unix.Timeval{tv, tv})
}

// Held returns the names of the entries in the forwarder's directory dir whose lock a live
// forwarder holds, sorted. It checks dir as Dir does, never makes it, and returns nothing for a
// missing one.
func Held(dir string) ([]string, error) {
	exists, err := checkDir(dir)
	if err != nil || !exists {
		return nil, err
	}
	names, err := readNames(dir)
	if err != nil {
		return nil, err
	}
	var held []string
	for _, name := range names {
		if !ParseEntryName(name) {
			continue
		}
		lock, err := takeLock(filepath.Join(dir, name))
		if errors.Is(err, unix.EWOULDBLOCK) {
			held = append(held, name)
		}
		if lock != nil {
			lock.Close()
		}
	}
	slices.Sort(held)
	return held, nil
}

// RemoveFree removes every entry and birth in the forwarder's directory dir whose lock is free,
// young swapped sockets included: a reset says nothing of coreslot's is in use. It checks dir as
// Dir does, never makes it, and removes nothing from a missing one.
func RemoveFree(dir string) (int, error) {
	exists, err := checkDir(dir)
	if err != nil || !exists {
		return 0, err
	}
	return eachFree(dir, func(string, bool) bool { return true })
}

// eachFree reads at most maxNames names of dir and, for each entry or birth whose lock it takes,
// removes it when remove says so, the lock still held. It returns how many it removed.
func eachFree(dir string, remove func(path string, birth bool) bool) (int, error) {
	names, err := readNames(dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, name := range names {
		base, birth := strings.CutPrefix(name, NewPrefix)
		if !ParseEntryName(base) {
			continue
		}
		path := filepath.Join(dir, name)
		lock, err := takeLock(path)
		if err != nil {
			if lock != nil {
				lock.Close()
			}
			continue
		}
		if remove(path, birth) && os.RemoveAll(path) == nil {
			removed++
		}
		lock.Close()
	}
	return removed, nil
}

// takeLock takes the lock of the entry at path without waiting, when path is a directory of the
// user's. The error wraps EWOULDBLOCK when a live process holds it. Closing the file releases it.
func takeLock(path string) (*os.File, error) {
	if err := store.CheckEntry(path, true); err != nil {
		return nil, err
	}
	lock, err := store.OpenFile(filepath.Join(path, LockName), unix.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return lock, err
	}
	return lock, nil
}

// readNames reads at most maxNames names of dir, without following a symlink at dir.
func readNames(dir string) ([]string, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: dir, Err: err}
	}
	f := os.NewFile(uintptr(fd), dir)
	defer f.Close()
	names, err := f.Readdirnames(maxNames)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return names, err
}
