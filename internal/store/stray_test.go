//go:build unix

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// strayEntries places, in the directory dir of the layout, one entry per way the PROTOCOL.md
// section 4.2 checks can fail under a name that matches the directory's grammar (and one under a
// temporary name): a symlink, a directory, a group-writable file. It returns their paths.
func strayEntries(t *testing.T, dir, dirName string) []string {
	t.Helper()
	var name func(seq int) string
	switch dirName {
	case QueueDir:
		name = ticketName
	case HoldsDir:
		name = func(seq int) string { return wire.HoldName(1791720600000000000, 7300, seq, "stray") }
	case CancelDir:
		name = func(seq int) string {
			return wire.ID{OrderKey: 1791720000000000000, PID: 4800, Seq: seq}.String()
		}
	}
	d := filepath.Join(dir, dirName)
	link := filepath.Join(d, name(901))
	if err := os.Symlink(filepath.Join(dir, AdmitLock), link); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(d, name(902))
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	gw := filepath.Join(d, name(903))
	if err := os.WriteFile(gw, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gw, 0o620); err != nil {
		t.Fatal(err)
	}
	tmpLink := filepath.Join(d, tmpName(4800, 904))
	if err := os.Symlink(filepath.Join(dir, AdmitLock), tmpLink); err != nil {
		t.Fatal(err)
	}
	return []string{link, sub, gw, tmpLink}
}

// A stray (PROTOCOL.md section 4.2): an entry of queue/, holds/ or cancel/ whose name matches the
// grammar but whose type, owner or mode fails the section 4.2 checks is ignored and counted like a
// malformed body, and reconciliation skips it and still writes gen.
func TestStrayEntriesAreIgnoredCountedAndSurviveReconciliation(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	live, err := sec.Publish(QueueDir, ticketName(1), []byte(`{}`), true)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	dead, err := sec.Publish(HoldsDir, wire.HoldName(1791720600000000000, 7300, 1, "good"), []byte(`{}`), true)
	if err != nil {
		t.Fatal(err)
	}
	dead.Close() // a dead hold
	if _, err := sec.Publish(CancelDir, wire.ID{OrderKey: 1791720000000000000, PID: 4712, Seq: 1}.String(), []byte(`{}`), false); err != nil {
		t.Fatal(err)
	}

	var strays []string
	for _, d := range []string{QueueDir, HoldsDir, CancelDir} {
		strays = append(strays, strayEntries(t, dir, d)...)
	}

	for _, d := range []string{QueueDir, HoldsDir, CancelDir} {
		entries, ignored, err := s.List(d)
		if err != nil {
			t.Fatalf("List(%s) with stray entries: %v", d, err)
		}
		if len(entries) != 1 || ignored != 4 {
			t.Errorf("List(%s) = %d entries, %d ignored; want 1 and 4", d, len(entries), ignored)
		}
	}

	now := time.Now().UnixNano()
	removed, err := sec.ReconcileIfDue(now)
	if err != nil {
		t.Fatalf("ReconcileIfDue with stray entries: %v", err)
	}
	// The dead hold goes; the cancel request goes with no ticket of its id in queue/. The live
	// ticket stays, and so does every stray.
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (the dead hold and the orphaned cancel request)", removed)
	}
	for _, p := range strays {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("reconciliation removed the stray entry %s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, AdmitLock)); err != nil {
		t.Errorf("the target of a stray symlink was touched: %v", err)
	}
	if g, err := s.ReadGen(); err != nil || g.ReconciledNS != now || g.N != 1 {
		t.Errorf("gen after reconciliation = %+v, %v; want reconciled_ns %d and gen 1", g, err, now)
	}

	// The next section entry works, and the stray entries are still only counted.
	f, err := sec.Publish(QueueDir, ticketName(2), []byte(`{}`), true)
	if err != nil {
		t.Fatalf("Publish after reconciliation: %v", err)
	}
	defer f.Close()
	if _, ignored, err := s.List(QueueDir); err != nil || ignored != 4 {
		t.Errorf("List(queue) after Publish = ignored %d, %v; want 4", ignored, err)
	}
}

// A stray in $S itself, outside the three directories, keeps the state-error rule.
func TestStrayTopLevelTemporaryStaysStateError(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	if err := os.Symlink(filepath.Join(dir, AdmitLock), filepath.Join(dir, tmpName(4800, 1))); err != nil {
		t.Fatal(err)
	}
	_, err := sec.ReconcileIfDue(time.Now().UnixNano())
	wantStateError(t, err, "reconciliation with a symlink under a temporary name in $S")
}

// The directories themselves keep the state-error rule.
func TestStrayDirectoryStaysStateError(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	if err := os.Symlink(filepath.Join(dir, ResDir), filepath.Join(dir, QueueDir)); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.List(QueueDir)
	wantStateError(t, err, "List of a queue/ that is a symlink")
}

// A stray never protects a cancel request: a queue/ entry ignored under the owner's stray rule
// is not an entry for any purpose, so a cancel request whose only grammar-matching queue/
// match is a stray (a symlink, a directory, a group-writable file named like the ticket) is
// removed by reconciliation step 2, while one whose ticket is a real live entry stays.
func TestStrayNeverProtectsACancelRequest(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	live, err := sec.Publish(QueueDir, ticketName(1), []byte(`{}`), true)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	strays := strayEntries(t, dir, QueueDir) // ticketName(901), ticketName(902), ticketName(903), a temporary
	idOf := func(seq int) wire.ID { return wire.ID{OrderKey: 1791720000000000000, PID: 4711, Seq: seq} }
	cancelPath := func(seq int) string { return filepath.Join(dir, CancelDir, idOf(seq).String()) }
	for _, seq := range []int{1, 901, 902, 903} {
		if _, err := sec.Publish(CancelDir, idOf(seq).String(), []byte(`{}`), false); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := sec.ReconcileIfDue(time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3 (the cancel requests whose only match is a stray)", removed)
	}
	for _, seq := range []int{901, 902, 903} {
		if _, err := os.Lstat(cancelPath(seq)); !os.IsNotExist(err) {
			t.Errorf("cancel request %d, whose only queue/ match is a stray, was kept: %v", seq, err)
		}
	}
	if _, err := os.Lstat(cancelPath(1)); err != nil {
		t.Errorf("the cancel request of a live ticket was removed: %v", err)
	}
	for _, p := range strays {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("reconciliation removed the stray entry %s: %v", p, err)
		}
	}
}
