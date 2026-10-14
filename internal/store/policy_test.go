//go:build unix

package store

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/wire"
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
		"admit.lock": 0o600,
	}
	checkModes := func(want map[string]os.FileMode) {
		t.Helper()
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
	}
	checkModes(want)
	// Before FORMAT is read, Open creates nothing else (PROTOCOL.md sections 14 and 15.2).
	if got := names(t, dir); !slices.Equal(got, []string{"admit.lock", "res"}) {
		t.Errorf("Open created %v, want only admit.lock and res", got)
	}
	// A second Open accepts the layout it created, and keeps admit.lock's inode.
	before, _ := os.Stat(filepath.Join(dir, "admit.lock"))
	s := open(t, dir)
	after, _ := os.Stat(filepath.Join(dir, "admit.lock"))
	if !os.SameFile(before, after) {
		t.Error("a second Open replaced admit.lock")
	}
	// The other directories appear, mode 0700, when an operation first needs them.
	sec := enter(t, s)
	for _, d := range []string{QueueDir, HoldsDir} {
		f, err := sec.Publish(d, ticketName(1), []byte(`{}`), true)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if _, err := sec.Publish(CancelDir, wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String(), []byte(`{}`), false); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRun(RunRecord{}); err != nil {
		t.Fatal(err)
	}
	checkModes(map[string]os.FileMode{"queue": os.ModeDir | 0o700, "holds": os.ModeDir | 0o700,
		"cancel": os.ModeDir | 0o700, "log": os.ModeDir | 0o700, "log/runs.jsonl": 0o600})
}

func TestOpenUnderUnknownFormatAddsNothing(t *testing.T) {
	// A directory written by a format-2 client: the format-stable files and FORMAT only.
	dir := newDir(t)
	if err := os.MkdirAll(filepath.Join(dir, ResDir), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{AdmitLock: "", FormatFile: "coreslot-protocol 2\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshot(t, dir)
	s := open(t, dir)
	sec := enter(t, s)
	if n, understood, err := sec.Format(); n != 2 || understood || err != nil {
		t.Fatalf("Format = %d, %v, %v", n, understood, err)
	}
	// What a token-only client reads must work without the directories it may not create.
	if e, ignored, err := s.List(QueueDir); len(e) != 0 || ignored != 0 || err != nil {
		t.Errorf("List of an absent queue/ = %v, %d, %v", e, ignored, err)
	}
	if n, err := sec.CountLocked("cpu"); n != 0 || err != nil {
		t.Errorf("CountLocked with no res/cpu = %d, %v", n, err)
	}
	if err := sec.rotateLogIfNeeded(); err != nil {
		t.Errorf("rotateLogIfNeeded with no log/: %v", err)
	}
	if d, err := s.Degraded(); d != nil || err != nil {
		t.Errorf("Degraded with no record = %v, %v", d, err)
	}
	if g, err := s.ReadGen(); g != (Gen{}) || err != nil {
		t.Errorf("ReadGen with no gen = %v, %v", g, err)
	}
	if after := snapshot(t, dir); after != before {
		t.Errorf("Open under an unknown format changed the directory:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestOpenChecksFilesystemBeforeCreating(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "a", "b", "state")
	var checked []string
	old := checkFilesystem
	checkFilesystem = func(p string) error {
		checked = append(checked, p)
		return &StateError{Reason: "refused for the test"}
	}
	t.Cleanup(func() { checkFilesystem = old })
	_, err := Open(dir)
	wantStateError(t, err, "Open on a refused filesystem")
	if !slices.Equal(checked, []string{parent}) {
		t.Errorf("checked %v, want the nearest existing ancestor %s first", checked, parent)
	}
	if exists(filepath.Join(parent, "a")) {
		t.Error("Open created directories on a refused filesystem")
	}
}

func TestOpenRejectsSymlinkedQueue(t *testing.T) {
	dir := newDir(t)
	open(t, dir)
	elsewhere := t.TempDir()
	q := filepath.Join(dir, "queue")
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
			if name == "holds" { // Open does not create it
				if err := os.Mkdir(p, 0o700); err != nil {
					t.Fatal(err)
				}
			}
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
		0x786F4256: true,  // vboxsf
		0x00C36400: true,  // Ceph
		0x5346414F: true,  // AFS
		0x6B414653: true,  // kAFS
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
