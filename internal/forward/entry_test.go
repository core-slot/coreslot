//go:build unix

package forward

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/docker/dockertest"
	"golang.org/x/sys/unix"
)

// lockedByOther is whether another description holds the lock file at path. It may run on a
// goroutine of the test's own, so it fails the test without stopping it.
func lockedByOther(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Errorf("open %s: %v", path, err)
		return false
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return false
	}
	if !errors.Is(err, unix.EWOULDBLOCK) {
		t.Errorf("flock %s: %v", path, err)
	}
	return true
}

// A watcher polling the directory sees an entry's final name only while its lock is held: Birth
// holds the rename until the watcher has looked. Mutation: renaming before the lock is taken
// lets the watcher take it.
func TestEntryVisibleOnlyLocked(t *testing.T) {
	parent := dockertest.SocketDir(t)
	entry := filepath.Join(parent, "7-k2")
	renamed, seen := make(chan struct{}), make(chan bool)
	rename = func(from, to string) error {
		err := os.Rename(from, to)
		close(renamed)
		if !<-seen {
			t.Error("the entry had its final name while its lock was free")
		}
		return err
	}
	t.Cleanup(func() { rename = os.Rename })
	go func() {
		<-renamed
		for {
			names, _ := os.ReadDir(parent)
			for _, n := range names {
				if !strings.HasPrefix(n.Name(), NewPrefix) {
					seen <- lockedByOther(t, filepath.Join(parent, n.Name(), LockName))
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	lock, ln, err := Birth(entry)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer ln.Close()
}

// The socket is 0600 and its directory 0700 under a umask of 0022, which the test sets: the library
// chmods, it never changes the umask. Mutation: dropping the socket's chmod leaves it 0755.
func TestSocketMode0600UnderUmask0022(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	entry := filepath.Join(dockertest.SocketDir(t), "7-k2")
	lock, ln, err := Birth(entry)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer ln.Close()
	sock, err := os.Lstat(filepath.Join(entry, SockName))
	if err != nil || sock.Mode()&os.ModeSocket == 0 || sock.Mode().Perm() != 0o600 {
		t.Errorf("socket %v, %v; want a socket of mode 0600", sock.Mode(), err)
	}
	dir, err := os.Lstat(entry)
	if err != nil || !dir.IsDir() || dir.Mode().Perm() != 0o700 {
		t.Errorf("entry %v, %v; want a directory of mode 0700", dir.Mode(), err)
	}
	if lf, err := os.Lstat(filepath.Join(entry, LockName)); err != nil || !lf.Mode().IsRegular() || lf.Mode().Perm() != 0o600 {
		t.Errorf("lock %v, %v; want a regular file of mode 0600", lf.Mode(), err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(entry), NewPrefix+"7-k2")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the birth directory is still there: %v", err)
	}
}

// A swap whose entry was removed recreates it: a 0700 directory, a lock nobody holds, and the
// address a symlink through which the engine answers. Mutation: a swap that needs the entry
// fails here.
func TestSwapRecreatesRemovedEntry(t *testing.T) {
	d := dockertest.HTTP(t)
	entry := filepath.Join(dockertest.SocketDir(t), "7-k2")
	lock, ln, err := Birth(entry)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	lock.Close()
	if err := os.RemoveAll(entry); err != nil {
		t.Fatal(err)
	}
	if err := Swap(entry, d.Sock); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if fi, err := os.Lstat(entry); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("entry %v, %v; want a 0700 directory", fi, err)
	}
	if lockedByOther(t, filepath.Join(entry, LockName)) {
		t.Error("the recreated lock is held")
	}
	addr := filepath.Join(entry, SockName)
	if target, err := os.Readlink(addr); err != nil || target != d.Sock {
		t.Errorf("address %q, %v; want a symlink to %q", target, err, d.Sock)
	}
	if st, _ := must(t, client(t, addr), "GET", "/info", nil); st != "200 OK" || d.Conns() != 1 {
		t.Errorf("a call through the recreated address: %s, %d connections at the engine; want 200 OK over 1", st, d.Conns())
	}
}
