//go:build unix

package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// newDir returns a fresh, not yet existing state directory path under the test's temp dir.
func newDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state dir") // a space, like Application Support
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	return s
}

func wantStateError(t *testing.T, err error, what string) {
	t.Helper()
	var se *StateError
	if !errors.As(err, &se) {
		t.Fatalf("%s: got %v (%T), want a *StateError", what, err, err)
	}
	if se.Reason == "" {
		t.Fatalf("%s: *StateError with an empty Reason", what)
	}
}

func TestOpenCreatesLayoutWithModes(t *testing.T) {
	dir := newDir(t)
	open(t, dir)
	want := map[string]os.FileMode{
		".":          os.ModeDir | 0o700,
		"res":        os.ModeDir | 0o700,
		"queue":      os.ModeDir | 0o700,
		"holds":      os.ModeDir | 0o700,
		"cancel":     os.ModeDir | 0o700,
		"log":        os.ModeDir | 0o700,
		"admit.lock": 0o600,
	}
	for name, mode := range want {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if fi.Mode() != mode {
			t.Errorf("%s: mode %v, want %v", name, fi.Mode(), mode)
		}
	}
	// A second Open accepts the layout it created, and keeps admit.lock's inode.
	before, _ := os.Stat(filepath.Join(dir, "admit.lock"))
	open(t, dir)
	after, _ := os.Stat(filepath.Join(dir, "admit.lock"))
	if !os.SameFile(before, after) {
		t.Error("a second Open replaced admit.lock")
	}
}

func TestOpenRejectsSymlinkedQueue(t *testing.T) {
	dir := newDir(t)
	open(t, dir)
	elsewhere := t.TempDir()
	q := filepath.Join(dir, "queue")
	if err := os.Remove(q); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, q); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	wantStateError(t, err, "Open with queue/ a symlink to a directory")

	// The state directory itself as a symlink is refused too.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Remove(q); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	_, err = Open(link)
	wantStateError(t, err, "Open of a symlinked state directory")
}

func TestOpenRejectsGroupWritable(t *testing.T) {
	for _, name := range []string{".", "holds", "admit.lock"} {
		t.Run(name, func(t *testing.T) {
			dir := newDir(t)
			open(t, dir)
			p := filepath.Join(dir, name)
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, fi.Mode().Perm()|0o020); err != nil {
				t.Fatal(err)
			}
			_, err = Open(dir)
			wantStateError(t, err, "Open with group-writable "+name)
		})
	}
	// config.toml under the override is checked like every other entry.
	dir := newDir(t)
	open(t, dir)
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("[resources]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg, 0o602); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	wantStateError(t, err, "Open with a world-writable config.toml")
	wantStateError(t, CheckEntry(cfg, false), "CheckEntry of a world-writable config.toml")
	if err := os.Chmod(cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckEntry(cfg, false); err != nil {
		t.Errorf("CheckEntry of a 0600 config.toml: %v", err)
	}
	if err := CheckEntry(filepath.Join(dir, "absent.toml"), false); err != nil {
		t.Errorf("CheckEntry of an absent path: %v, want nil", err)
	}
}

func TestOpenRejectsForeignFile(t *testing.T) {
	dir := newDir(t)
	open(t, dir)
	gen := filepath.Join(dir, "gen")
	if err := unix.Mkfifo(gen, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir) // must not block opening the fifo
	wantStateError(t, err, "Open with a fifo at gen")
	_, err = OpenFile(gen, os.O_RDONLY)
	wantStateError(t, err, "OpenFile of a fifo")

	// A directory where a file belongs, and a file where a directory belongs.
	dir2 := newDir(t)
	open(t, dir2)
	if err := os.Mkdir(filepath.Join(dir2, "FORMAT"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = Open(dir2)
	wantStateError(t, err, "Open with a directory at FORMAT")

	dir3 := newDir(t)
	open(t, dir3)
	if err := os.Remove(filepath.Join(dir3, "cancel")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir3, "cancel"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(dir3)
	wantStateError(t, err, "Open with a regular file at cancel")

	// A symlinked token file is refused by OpenFile (O_NOFOLLOW) and by Open.
	dir4 := newDir(t)
	open(t, dir4)
	if err := os.Mkdir(filepath.Join(dir4, "res", "cpu"), 0o700); err != nil {
		t.Fatal(err)
	}
	tok := filepath.Join(dir4, "res", "cpu", "0")
	if err := os.Symlink(filepath.Join(dir4, "admit.lock"), tok); err != nil {
		t.Fatal(err)
	}
	_, err = OpenFile(tok, os.O_RDONLY|os.O_CREATE)
	wantStateError(t, err, "OpenFile of a symlinked token")
	_, err = Open(dir4)
	wantStateError(t, err, "Open with a symlinked token")
}

func TestOpenFileIsCloseOnExecAndPrivate(t *testing.T) {
	dir := newDir(t)
	open(t, dir)
	p := filepath.Join(dir, "res", "probe")
	f, err := OpenFile(p, os.O_RDWR|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Error("OpenFile returned a descriptor without FD_CLOEXEC")
	}
	fl, err := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fl&unix.O_NONBLOCK != 0 {
		t.Error("OpenFile left O_NONBLOCK set")
	}
	fi, _ := f.Stat()
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("created mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestFilesystemTypeTable(t *testing.T) {
	types := map[string]bool{
		"nfs": true, "smbfs": true, "afpfs": true, "webdav": true,
		"macfuse": true, "macfuse_ntfs": true, "osxfuse": true, "osxfusefs": true,
		"apfs": false, "hfs": false, "tmpfs": false, "devfs": false, "msdos": false, "": false,
		"nfsx": false, "fuse": false,
	}
	for name, want := range types {
		if got := refusedType(name); got != want {
			t.Errorf("refusedType(%q) = %v, want %v", name, got, want)
		}
	}
	magics := map[int64]bool{
		0x6969: true, 0xFF534D42: true, 0xFE534D42: true, 0x517B: true, 0x65735546: true,
		0x01021997: true,
		0xEF53:     false, // ext4
		0x58465342: false, // xfs
		0x01021994: false, // tmpfs
		0x9123683E: false, // btrfs
		0x794C7630: false, // overlayfs
	}
	for m, want := range magics {
		if got := refusedMagic(m); got != want {
			t.Errorf("refusedMagic(%#x) = %v, want %v", m, got, want)
		}
	}
	if err := CheckFilesystem(t.TempDir()); err != nil {
		t.Errorf("CheckFilesystem of the local temp dir: %v", err)
	}
	if err := CheckFilesystem(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("CheckFilesystem of an absent path: nil error")
	}
}

func TestOpenRejectsRelativePath(t *testing.T) {
	_, err := Open("relative/state")
	wantStateError(t, err, "Open of a relative path")
}
