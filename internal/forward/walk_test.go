//go:build unix

package forward

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/store"
)

const day = 24 * time.Hour

// fwDir is a forwarder's directory made by Dir in a short base of its own.
func fwDir(t *testing.T) string {
	t.Helper()
	dir, err := Dir(realBase(t))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// The kinds of docker.sock an entry can hold.
const (
	deadSocket = "socket" // bound and closed: nobody serves it
	swapped    = "symlink"
	regular    = "file"
	none       = "none"
)

// mkEntry makes the entry name in dir with a free lock and a docker.sock of kind; a symlink is
// backdated to age by its own time, while its target stays new.
func mkEntry(t *testing.T, dir, name, kind string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := store.OpenFile(filepath.Join(path, LockName), unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	sock := filepath.Join(path, SockName)
	switch kind {
	case deadSocket:
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		ln.Close()
	case swapped:
		target := filepath.Join(dir, "..", "upstream.sock")
		if err := os.WriteFile(target, nil, 0o600); err != nil && !errors.Is(err, fs.ErrExist) {
			t.Fatal(err)
		}
		if err := os.Symlink(target, sock); err != nil {
			t.Fatal(err)
		}
		then := unix.NsecToTimespec(time.Now().Add(-age).UnixNano())
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, sock, []unix.Timespec{then, then}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			t.Fatal(err)
		}
	case regular:
		if err := os.WriteFile(sock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// holdLock makes a child process the only holder of the entry's lock until the test ends.
func holdLock(t *testing.T, entry string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(entry, LockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.ExtraFiles = []*os.File{f}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	f.Close() // the child's copy of the description holds the lock now
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
}

// exists reports whether path is there, by Lstat.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// walk runs a due Walk and checks how many entries it removed.
func walk(t *testing.T, dir string, now time.Time, want int) {
	t.Helper()
	removed, err := Walk(dir, now)
	if err != nil || removed != want {
		t.Fatalf("Walk = %d, %v; want %d removed", removed, err, want)
	}
}

// An entry whose lock is free and whose socket nobody serves is removed. Mutation: remove only
// symlinks → the entry stays, red.
func TestWalkRemovesDeadSocketEntry(t *testing.T) {
	dir := fwDir(t)
	e := mkEntry(t, dir, EntryName(4711, 1), deadSocket, 0)
	walk(t, dir, time.Now(), 1)
	if exists(e) {
		t.Error("the dead entry is still there")
	}
}

// An entry whose lock a live process holds is a forwarder still serving: kept. Mutation: skip
// the lock → removed, red.
func TestWalkKeepsHeldEntry(t *testing.T) {
	dir := fwDir(t)
	e := mkEntry(t, dir, EntryName(4711, 1), deadSocket, 0)
	holdLock(t, e)
	walk(t, dir, time.Now(), 0)
	if !exists(e) {
		t.Error("the held entry was removed")
	}
}

// A swapped socket stays for 30 days by its own time, whatever its target's. Mutation: compare
// the target's mtime (os.Stat) → the 31-day entry stays, red.
func TestWalkSymlinkAge(t *testing.T) {
	dir := fwDir(t)
	young := mkEntry(t, dir, EntryName(1, 29), swapped, 29*day)
	old := mkEntry(t, dir, EntryName(1, 31), swapped, 31*day)
	walk(t, dir, time.Now(), 1)
	if !exists(young) || exists(old) {
		t.Errorf("after the walk: 29-day entry there %v, 31-day entry there %v; want true, false", exists(young), exists(old))
	}
}

// A birth that died before its rename is removed when its lock is free, whatever it holds; one
// in progress (its lock held) is kept. Mutation: ignore the prefix → the dead birth stays, red.
func TestWalkRemovesDeadBirth(t *testing.T) {
	dir := fwDir(t)
	dead := mkEntry(t, dir, NewPrefix+EntryName(4711, 1), none, 0)
	live := mkEntry(t, dir, NewPrefix+EntryName(4712, 1), deadSocket, 0)
	holdLock(t, live)
	walk(t, dir, time.Now(), 1)
	if exists(dead) || !exists(live) {
		t.Errorf("dead birth there %v, live birth there %v; want false, true", exists(dead), exists(live))
	}
}

// The walk runs at most once an hour for the machine, by the stamp's time. Mutation: walk on
// every call → the second walk removes, red.
func TestWalkOncePerHour(t *testing.T) {
	dir := fwDir(t)
	t0 := time.Now()
	mkEntry(t, dir, EntryName(1, 1), deadSocket, 0)
	walk(t, dir, t0, 1)
	e := mkEntry(t, dir, EntryName(1, 2), deadSocket, 0)
	walk(t, dir, t0.Add(59*time.Minute), 0)
	if !exists(e) {
		t.Fatal("a walk within the hour removed an entry")
	}
	walk(t, dir, t0.Add(61*time.Minute), 1)
}

// A walk reads at most 65 536 names, one of them perhaps its own stamp, processes those, and is
// no error; the next due walk removes the rest. Mutation: read every name → 65 600 at once, red.
func TestWalkProcessesFirst65536(t *testing.T) {
	if testing.Short() {
		t.Skip("makes 65 600 entries")
	}
	dir := fwDir(t)
	const n = maxNames + 64
	for i := range n {
		path := filepath.Join(dir, NewPrefix+EntryName(i+1, 1))
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, LockName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now()
	first, err := Walk(dir, t0)
	if err != nil || first < maxNames-1 || first > maxNames {
		t.Fatalf("first Walk = %d, %v; want 65 535 or 65 536 removed (the stamp may be one of the names)", first, err)
	}
	second, err := Walk(dir, t0.Add(2*time.Hour))
	if err != nil || first+second != n {
		t.Fatalf("second Walk = %d, %v; want the other %d", second, err, n-first)
	}
}

// Whatever is not an entry the forwarder makes is left alone: a regular file as docker.sock, an
// entry with no docker.sock, an entry with no lock, an entry that is itself a symlink, and a name
// that is not an entry's. Mutation: remove every entry whose lock is free → the regular file's
// entry goes, red.
func TestWalkLeavesForeignTypes(t *testing.T) {
	dir := fwDir(t)
	keep := []string{
		mkEntry(t, dir, EntryName(1, 1), regular, 0),
		mkEntry(t, dir, EntryName(1, 2), none, 0),
		mkEntry(t, dir, "not-an-entry", deadSocket, 0),
	}
	noLock := filepath.Join(dir, EntryName(1, 3))
	if err := os.Mkdir(noLock, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(noLock, SockName))
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	target := mkEntry(t, filepath.Dir(dir), "elsewhere", deadSocket, 0)
	link := filepath.Join(dir, EntryName(1, 4))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	walk(t, dir, time.Now(), 0)
	for _, p := range append(keep, noLock, link, filepath.Join(target, SockName)) {
		if !exists(p) {
			t.Errorf("%s was removed", p)
		}
	}
}

// Held lists the entries a live process holds; RemoveFree removes every entry whose lock is free,
// young swapped sockets and dead births included, and keeps the held one. Neither makes a missing
// directory, and both refuse one that is a symlink. Mutations: let Held list free entries → red;
// let RemoveFree keep young symlinks → red; make the directory when missing → red.
func TestHeldAndRemoveFree(t *testing.T) {
	dir := fwDir(t)
	held := EntryName(4711, 1)
	holdLock(t, mkEntry(t, dir, held, deadSocket, 0))
	mkEntry(t, dir, EntryName(4712, 1), deadSocket, 0)
	mkEntry(t, dir, EntryName(4713, 1), swapped, time.Hour)
	mkEntry(t, dir, NewPrefix+EntryName(4714, 1), none, 0)
	got, err := Held(dir)
	if err != nil || !slices.Equal(got, []string{held}) {
		t.Errorf("Held = %q, %v; want [%s]", got, err, held)
	}
	n, err := RemoveFree(dir)
	if err != nil || n != 3 {
		t.Errorf("RemoveFree = %d, %v; want 3", n, err)
	}
	names, _ := os.ReadDir(dir)
	if len(names) != 1 || names[0].Name() != held {
		t.Errorf("left %v; want only %s", names, held)
	}

	missing := filepath.Join(realBase(t), DirName)
	if got, err := Held(missing); got != nil || err != nil {
		t.Errorf("Held(missing) = %q, %v", got, err)
	}
	if n, err := RemoveFree(missing); n != 0 || err != nil {
		t.Errorf("RemoveFree(missing) = %d, %v", n, err)
	}
	if exists(missing) {
		t.Error("a missing directory was made")
	}
	link := filepath.Join(realBase(t), DirName)
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Held(link); err == nil {
		t.Error("Held followed a symlinked directory")
	}
	if _, err := RemoveFree(link); err == nil {
		t.Error("RemoveFree followed a symlinked directory")
	}
}
