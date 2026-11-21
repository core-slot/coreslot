//go:build unix

package forward

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/docker/dockertest"
	"github.com/core-slot/coreslot/internal/store"
)

// realBase is a short base directory, by its real path.
func realBase(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(dockertest.SocketDir(t))
	if err != nil {
		t.Fatal(err)
	}
	return base
}

// The directory is made once with mode 0700; a second call keeps it, and the entries in it.
// Mutation: remove and remake an existing directory → the entry is gone, red.
func TestDirMadeOnce0700(t *testing.T) {
	base := realBase(t)
	dir, err := Dir(base)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, DirName); dir != want {
		t.Fatalf("Dir = %q, want %q", dir, want)
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("Lstat(%s) = %v, %v; want a directory of mode 0700", dir, fi, err)
	}
	entry := filepath.Join(dir, "7-k2")
	if err := os.Mkdir(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	again, err := Dir(base)
	if err != nil || again != dir {
		t.Fatalf("second Dir = %q, %v", again, err)
	}
	if _, err := os.Lstat(entry); err != nil {
		t.Errorf("the second Dir lost the entry: %v", err)
	}
}

// The address is the socket's real path, so the directory is named by its base's real path: a
// base reached through a symlink gives the directory under the symlink's target. Mutation: join
// the base as given → the symlink's path, red.
func TestDirIsRealPath(t *testing.T) {
	base := realBase(t)
	link := base + "-link"
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(link) })
	dir, err := Dir(link)
	if want := filepath.Join(base, DirName); err != nil || dir != want {
		t.Errorf("Dir(%s) = %q, %v; want %q", link, dir, err, want)
	}
}

// Anything but a real directory of the user's with no group or world write is refused, and what
// is there is left as it was: a regular file, a symlink (also one to a directory of the user's), a
// group- or world-writable directory, a base the directory cannot be made in (what an agent
// sandbox gives) and a base that is not absolute. Mutation: os.Stat for os.Lstat in the entry
// check → the row of a symlink to the user's directory, red.
func TestDirRefused(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, base string){
		"regular file": func(t *testing.T, base string) {
			if err := os.WriteFile(filepath.Join(base, DirName), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink to the user's directory": func(t *testing.T, base string) {
			target := filepath.Join(base, "target")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(base, DirName)); err != nil {
				t.Fatal(err)
			}
		},
		"dangling symlink": func(t *testing.T, base string) {
			if err := os.Symlink(filepath.Join(base, "nowhere"), filepath.Join(base, DirName)); err != nil {
				t.Fatal(err)
			}
		},
		"group-writable": func(t *testing.T, base string) { mkdirMode(t, filepath.Join(base, DirName), 0o770) },
		"world-writable": func(t *testing.T, base string) { mkdirMode(t, filepath.Join(base, DirName), 0o707) },
		"cannot be made": func(t *testing.T, base string) {
			if err := os.Chmod(base, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(base, 0o700) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := realBase(t)
			setup(t, base)
			before, _ := os.Lstat(filepath.Join(base, DirName))
			if dir, err := Dir(base); err == nil {
				t.Fatalf("Dir = %q, nil; want a refusal", dir)
			}
			after, _ := os.Lstat(filepath.Join(base, DirName))
			if (before == nil) != (after == nil) || before != nil && (before.Mode() != after.Mode() || !os.SameFile(before, after)) {
				t.Errorf("Dir changed what was there: %v → %v", before, after)
			}
		})
	}
	if dir, err := Dir("relative/home"); err == nil {
		t.Errorf("Dir(a relative base) = %q, nil", dir)
	}
}

// mkdirMode makes a directory with exactly mode, whatever the umask.
func mkdirMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o700) })
}

// A base whose path has a space gets no directory, and an address over 96 bytes is refused while
// one of exactly 96 is taken. Mutations: drop the space check → the directory is made, red;
// compare with < 96 → the 96-byte row, red.
func TestAddressRefusedWithReason(t *testing.T) {
	spaced := filepath.Join(realBase(t), "a b")
	if err := os.Mkdir(spaced, 0o700); err != nil {
		t.Fatal(err)
	}
	if dir, err := Dir(spaced); err == nil {
		t.Errorf("Dir(%q) = %q, nil; want a refusal", spaced, dir)
	}
	if _, err := os.Lstat(filepath.Join(spaced, DirName)); err == nil {
		t.Error("Dir made the directory under a base with a space")
	}
	const name = "1-a" // the address is dir + "/1-a/docker.sock": len(dir) + 16 bytes
	for _, c := range []struct {
		dirLen int
		ok     bool
	}{{80, true}, {81, false}} {
		dir := "/" + strings.Repeat("d", c.dirLen-1)
		addr, ok := Address(dir, name)
		if ok != c.ok || addr != dir+"/1-a/docker.sock" {
			t.Errorf("Address(%d-byte dir) = %d bytes, %v; want ok %v", c.dirLen, len(addr), ok, c.ok)
		}
	}
}

// Two start times of one pid name two entries; the largest pid with a start in 2119 is 18 bytes;
// ParseEntryName takes every name EntryName makes and nothing that is not one. Mutations: format
// the start in base 10 → 25 bytes, red; accept upper-case digits → "1-A", red.
func TestEntryNameUniqueAndShort(t *testing.T) {
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC).UnixNano()
	if EntryName(4711, start) == EntryName(4711, start+1) {
		t.Error("two start times of one pid give one name")
	}
	late := time.Date(2119, 12, 31, 23, 59, 59, 999999999, time.UTC).UnixNano()
	if n := EntryName(99999, late); len(n) != 18 {
		t.Errorf("EntryName(99999, 2119) = %q, %d bytes; want 18", n, len(n))
	}
	for _, n := range []string{EntryName(1, 0), EntryName(4711, start), EntryName(99999, late)} {
		if !ParseEntryName(n) {
			t.Errorf("ParseEntryName(%q) = false", n)
		}
	}
	for _, n := range []string{"", "1", "1-", "-a", "1-A", "a-1", "1_a", ".new-1-a", "1-a-b", "+1-a", "1-a.", DirName, stampName} {
		if ParseEntryName(n) {
			t.Errorf("ParseEntryName(%q) = true", n)
		}
	}
}

// Under the state directory override the state directory stands for the home directory: the
// forwarder's directory is made in it, and the store still opens it, since the store never reads
// that name. Mutation: let the store refuse an unknown name → red.
func TestDirUnderOverride(t *testing.T) {
	s := realBase(t)
	dir, err := Dir(s)
	if err != nil || dir != filepath.Join(s, DirName) {
		t.Fatalf("Dir(%s) = %q, %v", s, dir, err)
	}
	if _, err := store.Open(s); err != nil {
		t.Errorf("store.Open beside the forwarder's directory: %v", err)
	}
}
