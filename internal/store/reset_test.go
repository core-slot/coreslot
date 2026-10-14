//go:build unix

package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/core-slot/coreslot/internal/wire"
)

// resetFixture fills a state directory with one of everything a reset removes (dead tickets
// and holds, a cancel request, a stray, a temporary file, the run log, degraded, gen, FORMAT)
// and everything it keeps (admit.lock, token files, config.toml). It returns the section the
// caller is in.
func resetFixture(t *testing.T, dir string) (*Store, *Section) {
	t.Helper()
	s := open(t, dir)
	sec := enter(t, s)
	if _, _, err := sec.Format(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		dir, name string
		body      string
	}{
		{QueueDir, ticketName(1), `{}`},
		{HoldsDir, wire.HoldName(1791720600000000000, 4711, 1, "gate"), `{}`},
		{CancelDir, wire.ID{OrderKey: 1791720000000000000, PID: 4711, Seq: 1}.String(), `{}`},
	} {
		f, err := sec.Publish(p.dir, p.name, []byte(p.body), false)
		if err != nil {
			t.Fatal(err)
		}
		if f != nil {
			f.Close()
		}
	}
	if err := os.Symlink(filepath.Join(dir, AdmitLock), filepath.Join(dir, QueueDir, ticketName(2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, QueueDir, tmpName(4711, 3)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sec.BumpGen(); err != nil {
		t.Fatal(err)
	}
	if err := sec.RecordDegraded("test"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, LogDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, LogDir, RunLog), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte("[resources]\ncpu = 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ResDir, "cpu"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(tokenPath(dir, "cpu", i), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return s, sec
}

// A reset of a state with nothing alive removes the entries of queue/, holds/, cancel/ and
// log/, degraded, gen and FORMAT, and keeps admit.lock, res/ (with the same inodes) and
// config.toml.
func TestResetRemovesEverythingButTheAnchors(t *testing.T) {
	dir := newDir(t)
	_, sec := resetFixture(t, dir)
	inode := func(p string) uint64 {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Sys().(*syscall.Stat_t).Ino
	}
	lockBefore, tokenBefore := inode(filepath.Join(dir, AdmitLock)), inode(tokenPath(dir, "cpu", 0))

	live, removed, err := sec.Reset()
	if err != nil || !live.Empty() {
		t.Fatalf("Reset = %+v, %d, %v; want nothing alive and no error", live, removed, err)
	}
	if removed != 9 {
		t.Errorf("removed %d entries, want 9 (ticket, stray and temporary file; hold; cancel request; log; degraded, gen, FORMAT)", removed)
	}
	for _, d := range []string{QueueDir, HoldsDir, CancelDir, LogDir} {
		if got := names(t, filepath.Join(dir, d)); len(got) != 0 {
			t.Errorf("%s/ still holds %v", d, got)
		}
	}
	for _, f := range []string{DegradedFile, GenFile, FormatFile} {
		if exists(filepath.Join(dir, f)) {
			t.Errorf("%s survived the reset", f)
		}
	}
	for _, f := range []string{AdmitLock, ConfigFile, filepath.Join(ResDir, "cpu", "0"), filepath.Join(ResDir, "cpu", "1")} {
		if !exists(filepath.Join(dir, f)) {
			t.Errorf("%s was removed", f)
		}
	}
	if got := inode(filepath.Join(dir, AdmitLock)); got != lockBefore {
		t.Errorf("admit.lock inode %d -> %d", lockBefore, got)
	}
	if got := inode(tokenPath(dir, "cpu", 0)); got != tokenBefore {
		t.Errorf("token inode %d -> %d", tokenBefore, got)
	}
}

// Anything alive stops the reset before it removes anything: a held ticket, a held hold, a
// locked token (of a resource the configuration does not even declare).
func TestResetRefusesWhatIsAlive(t *testing.T) {
	cases := []struct {
		name  string
		alive func(t *testing.T, dir string, sec *Section) // makes one thing alive
		check func(t *testing.T, live LiveState)
	}{
		{"ticket", func(t *testing.T, dir string, sec *Section) {
			f, err := sec.Publish(QueueDir, ticketName(10), []byte(`{}`), true)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { f.Close() })
		}, func(t *testing.T, live LiveState) {
			if len(live.Tickets) != 1 || live.Tickets[0] != ticketName(10) || len(live.Holds) != 0 || len(live.Tokens) != 0 {
				t.Errorf("live = %+v, want only ticket %s", live, ticketName(10))
			}
		}},
		{"hold", func(t *testing.T, dir string, sec *Section) {
			f, err := sec.Publish(HoldsDir, wire.HoldName(1791720600000000001, 4711, 10, "live"), []byte(`{}`), true)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { f.Close() })
		}, func(t *testing.T, live LiveState) {
			if len(live.Holds) != 1 || len(live.Tickets) != 0 || len(live.Tokens) != 0 {
				t.Errorf("live = %+v, want one hold", live)
			}
		}},
		{"token", func(t *testing.T, dir string, sec *Section) {
			if err := os.MkdirAll(filepath.Join(dir, ResDir, "old-gpu"), 0o700); err != nil {
				t.Fatal(err)
			}
			startHelper(t, "lock", dir, tokenPath(dir, "old-gpu", 3), nil)
		}, func(t *testing.T, live LiveState) {
			if live.Tokens["old-gpu"] != 1 || len(live.Holds) != 0 || len(live.Tickets) != 0 {
				t.Errorf("live = %+v, want one locked old-gpu token", live)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := newDir(t)
			_, sec := resetFixture(t, dir)
			c.alive(t, dir, sec)
			setOtherLengthHolder(t, dir)
			before := snapshotTree(t, dir)
			live, removed, err := sec.Reset()
			if err != nil || live.Empty() || removed != 0 {
				t.Fatalf("Reset = %+v, %d, %v; want what is alive, nothing removed", live, removed, err)
			}
			c.check(t, live)
			if after := snapshotTree(t, dir); after != before {
				t.Errorf("the state directory changed although the reset refused:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// A malformed FORMAT does not stop a reset: it is what repairs it.
func TestResetRepairsMalformedFormat(t *testing.T) {
	dir := newDir(t)
	_, sec := resetFixture(t, dir)
	if err := os.WriteFile(filepath.Join(dir, FormatFile), []byte("coreslot-protocol x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if live, _, err := sec.Reset(); err != nil || !live.Empty() {
		t.Fatalf("Reset = %+v, %v", live, err)
	}
	if exists(filepath.Join(dir, FormatFile)) {
		t.Error("the malformed FORMAT was not removed")
	}
}

// An entry that cannot be removed is reported, after the rest was removed.
func TestResetReportsWhatItCannotRemove(t *testing.T) {
	dir := newDir(t)
	_, sec := resetFixture(t, dir)
	stuck := filepath.Join(dir, QueueDir, ticketName(20))
	if err := os.Mkdir(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	setOtherLengthHolder(t, dir)
	_, removed, err := sec.Reset()
	if err == nil {
		t.Fatal("Reset left a non-empty directory behind without an error")
	}
	if removed != 9 {
		t.Errorf("removed %d entries alongside the error, want 9 (everything but the stuck entry)", removed)
	}
	var incomplete *ResetIncompleteError
	if !errors.As(err, &incomplete) || incomplete.Left != 1 {
		t.Errorf("error %v (%T): want a *ResetIncompleteError with one entry left", err, err)
	}
	if body, err := os.ReadFile(filepath.Join(dir, AdmitLock)); err != nil || recordedPID(body) != os.Getpid() {
		t.Errorf("admit.lock = %q, %v; a reset that removed things must have marked itself first", body, err)
	}
	if !exists(stuck) || exists(filepath.Join(dir, GenFile)) {
		t.Error("want the stuck entry kept and the other removals done")
	}
}

// snapshotTree is every path under dir with its mode, size and inode, and the bytes of every
// regular file (the target of every symlink): equal strings, equal trees. A size alone would
// pass a rewrite that happens to keep the length, so content is compared, and the inode catches
// a file replaced by an identical copy.
func snapshotTree(t *testing.T, dir string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&out, "%s %s size=%d ino=%d", p, fi.Mode(), fi.Size(), fi.Sys().(*syscall.Stat_t).Ino)
		switch {
		case fi.Mode().IsRegular():
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&out, " body=%q", body)
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&out, " -> %s", target)
		}
		out.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// recordedPID is the pid in admit.lock's informational body, 0 when it holds none.
func recordedPID(body []byte) int {
	var b struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(body, &b) != nil {
		return 0
	}
	return b.PID
}

// setOtherLengthHolder rewrites admit.lock's body, in place, as a record of a pid whose decimal
// form has a different number of digits than this process's, so that a reset which marks itself
// as holder changes the file's size as well as its content. On a platform whose pids wrap (macOS
// at 99999) this is the case a size comparison passes or fails by luck.
func setOtherLengthHolder(t *testing.T, dir string) {
	t.Helper()
	digits := len(fmt.Sprint(os.Getpid())) + 1
	pid := strings.Repeat("9", digits)
	body := fmt.Sprintf(`{"pid":%s,"entered_ns":1}`, pid)
	if err := os.WriteFile(filepath.Join(dir, AdmitLock), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A well-formed FORMAT this client does not implement is a newer client's directory: its
// entries may not be format-1 grammar and its waiters are invisible here, so the reset removes
// nothing. FORMAT 0 is well-formed too.
func TestResetRefusesForeignFormat(t *testing.T) {
	for _, body := range []string{"coreslot-protocol 2\n", "coreslot-protocol 0", "coreslot-protocol 007\n"} {
		t.Run(body, func(t *testing.T) {
			dir := newDir(t)
			_, sec := resetFixture(t, dir)
			if err := os.WriteFile(filepath.Join(dir, FormatFile), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			// An entry no format-1 grammar describes, as a newer format may keep.
			if err := os.WriteFile(filepath.Join(dir, QueueDir, "v2-ticket.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			setOtherLengthHolder(t, dir)
			before := snapshotTree(t, dir)
			live, removed, err := sec.Reset()
			if err != nil || removed != 0 || live.ForeignFormat == nil || live.Empty() {
				t.Fatalf("Reset = %+v, %d, %v; want a refusal naming the foreign format", live, removed, err)
			}
			if after := snapshotTree(t, dir); after != before {
				t.Errorf("the refused reset changed the tree:\nbefore %s\nafter  %s", before, after)
			}
			// The same directory is reset when this client implements that format.
			foreign := *live.ForeignFormat
			if live, _, err := sec.reset(func(n int) bool { return n == 1 || n == foreign }); err != nil || !live.Empty() {
				t.Errorf("Reset with the format implemented = %+v, %v", live, err)
			}
		})
	}
}

// A reset that fails before it removes anything (here a queue/ that is a symlink, which no
// client may follow) writes nothing either: the error path is a refusal too.
func TestResetErrorBeforeRemovalWritesNothing(t *testing.T) {
	dir := newDir(t)
	_, sec := resetFixture(t, dir)
	other := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, QueueDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(dir, QueueDir)); err != nil {
		t.Fatal(err)
	}
	setOtherLengthHolder(t, dir)
	before := snapshotTree(t, dir)
	if live, removed, err := sec.Reset(); err == nil || removed != 0 || !live.Empty() {
		t.Fatalf("Reset = %+v, %d, %v; want an error and nothing removed", live, removed, err)
	}
	if after := snapshotTree(t, dir); after != before {
		t.Errorf("the failed reset changed the tree:\nbefore %s\nafter  %s", before, after)
	}
}

// A reset that goes ahead records itself in admit.lock's body, so status names it meanwhile.
func TestResetMarksItselfHolder(t *testing.T) {
	dir := newDir(t)
	_, sec := resetFixture(t, dir)
	setOtherLengthHolder(t, dir)
	if _, _, err := sec.Reset(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, AdmitLock))
	if err != nil || recordedPID(body) != os.Getpid() {
		t.Errorf("admit.lock = %q, %v; want this process as holder", body, err)
	}
}

// OpenReadOnly creates nothing: not the state directory, not res/, not admit.lock.
func TestOpenReadOnlyCreatesNothing(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing", "state dir")
	s, err := OpenReadOnly(missing)
	if err != nil || s.Exists() {
		t.Fatalf("OpenReadOnly of a missing directory = %v, exists %v", err, s != nil && s.Exists())
	}
	if _, err := os.Lstat(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Errorf("OpenReadOnly created a parent: %v", err)
	}
	if held, err := s.SectionHeld(); held || err != nil {
		t.Errorf("SectionHeld of a missing directory = %v, %v", held, err)
	}
	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, empty)
	s, err = OpenReadOnly(empty)
	if err != nil || !s.Exists() {
		t.Fatalf("OpenReadOnly of an empty directory = %v", err)
	}
	if held, err := s.SectionHeld(); held || err != nil {
		t.Errorf("SectionHeld of an empty directory = %v, %v", held, err)
	}
	if res, err := s.Resources(); err != nil || len(res) != 0 {
		t.Errorf("Resources = %v, %v", res, err)
	}
	if after := snapshotTree(t, empty); after != before {
		t.Errorf("a read-only open and probe changed the directory:\nbefore %s\nafter  %s", before, after)
	}
	// It still applies the policy to what exists.
	bad := filepath.Join(root, "bad")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(empty, filepath.Join(bad, ResDir)); err != nil {
		t.Fatal(err)
	}
	_, err = OpenReadOnly(bad)
	wantStateError(t, err, "a symlinked res/")
}

// SectionHeld sees another description's hold without taking it.
func TestSectionHeldProbe(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	if held, err := s.SectionHeld(); held || err != nil {
		t.Fatalf("a free section: %v, %v", held, err)
	}
	sec := enter(t, s)
	if held, err := s.SectionHeld(); !held || err != nil {
		t.Errorf("a held section: %v, %v", held, err)
	}
	sec.Leave()
	if held, err := s.SectionHeld(); held || err != nil {
		t.Errorf("a left section: %v, %v", held, err)
	}
}
