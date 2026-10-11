//go:build unix

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// resetFixture fills a state directory with one of everything a reset removes (dead tickets
// and holds, a cancel request, a stray, a temporary file, the run log, degraded, gen, FORMAT)
// and everything it keeps (admit.lock, token files, config.toml). It returns the section the
// caller is in.
func resetFixture(t *testing.T, dir string) (*Store, *Section) {
	t.Helper()
	s := open(t, dir)
	sec := enter(t, s)
	if _, _, err := sec.Format([]int{1}); err != nil {
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
	if _, _, err := sec.Reset(); err == nil {
		t.Fatal("Reset left a non-empty directory behind without an error")
	}
	if !exists(stuck) || exists(filepath.Join(dir, GenFile)) {
		t.Error("want the stuck entry kept and the other removals done")
	}
}

// snapshotTree is every path under dir with its size and mode: equal strings, equal trees.
func snapshotTree(t *testing.T, dir string) string {
	t.Helper()
	var out string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out += p + " " + fi.Mode().String() + " " + fmt.Sprint(fi.Size()) + "\n"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
