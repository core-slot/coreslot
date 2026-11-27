//go:build unix

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/wire"
)

// A mark is published once, listed, and removed; a second mark of the same id finds it there. A
// symlinked entry is not trusted: Marks ignores it, and a symlinked mark directory is a state error.
// Mutations: leaving the mark directories out of noReplaceDirs refuses Mark; listing a stray entry in
// Marks lists the symlink; leaving them out of Open's checks opens a symlinked quiet/.
func TestMarks(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	defer sec.Leave()
	id := wire.ID{OrderKey: 1791720000000000000, PID: 4711, Seq: 1}
	for _, d := range []string{QuietDir, NoStopDir} {
		if already, err := sec.Mark(d, id); err != nil || already {
			t.Fatalf("Mark(%s) = %v, %v; want false, nil", d, already, err)
		}
		if already, err := sec.Mark(d, id); err != nil || !already {
			t.Errorf("a second Mark(%s) = %v, %v; want true, nil", d, already, err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, QuietDir, id.String()), filepath.Join(dir, QuietDir, wire.ID{Seq: 9}.String())); err != nil {
		t.Fatal(err)
	}
	if m, err := s.Marks(QuietDir); err != nil || len(m) != 1 || !m[id.String()] {
		t.Errorf("Marks(quiet) = %v, %v; want only %s", m, err, id)
	}
	if removed, err := sec.Unmark(QuietDir, id); err != nil || !removed {
		t.Errorf("Unmark = %v, %v; want true", removed, err)
	}
	if removed, err := sec.Unmark(QuietDir, id); err != nil || removed {
		t.Errorf("a second Unmark = %v, %v; want false, nil", removed, err)
	}
	if m, err := s.Marks(NoStopDir); err != nil || !m[id.String()] {
		t.Errorf("Marks(nostop) = %v, %v; the quiet mark's removal took it", m, err)
	}

	other := newDir(t)
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(other, QuietDir)); err != nil {
		t.Fatal(err)
	}
	_, err := Open(other)
	wantStateError(t, err, "a symlinked quiet/")
}

// Reconciliation removes a mark whose ticket and hold are both gone, keeps a live ticket's and a
// live hold's, and removes a stale temporary file; a reset empties both directories. Mutations:
// leaving the mark directories out of step 2 keeps the orphan; counting only tickets as live removes
// the hold's mark; leaving them out of step 3 keeps the temporary file; out of the reset keeps the
// marks.
func TestMarksReconciledAndReset(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	ticket, err := sec.Publish(QueueDir, ticketName(1), []byte(`{}`), true)
	if err != nil {
		t.Fatal(err)
	}
	defer ticket.Close()
	hold, holdName, err := sec.PublishHold(wire.Hold{PID: 4711, StartNS: 1791730000000000000, NeedMiB: 1}, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	ticketID, _ := wire.ParseName(ticketName(1))
	holdID, _ := wire.ParseName(holdName)
	orphan := wire.ID{OrderKey: 1791720000000000000, PID: 4711, Seq: 3}
	for _, d := range []string{QuietDir, NoStopDir} {
		for _, id := range []wire.ID{ticketID, holdID, orphan} {
			if _, err := sec.Mark(d, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	tmp := writeTmp(t, filepath.Join(dir, QuietDir), 4711, 7, time.Minute)
	if removed, err := sec.ReconcileIfDue(time.Now().UnixNano()); err != nil || removed != 3 {
		t.Errorf("reconcile removed %d, %v; want 3: two orphaned marks and a temporary file", removed, err)
	}
	for _, d := range []string{QuietDir, NoStopDir} {
		if m, err := s.Marks(d); err != nil || len(m) != 2 || !m[ticketID.String()] || !m[holdID.String()] {
			t.Errorf("%s after reconciliation: %v, %v; want the ticket's and the hold's", d, m, err)
		}
	}
	if exists(tmp) {
		t.Error("the stale temporary file of quiet/ was kept")
	}
	sec.Leave()

	rdir := newDir(t)
	_, rsec := resetFixture(t, rdir)
	for _, d := range []string{QuietDir, NoStopDir} {
		if _, err := rsec.Mark(d, orphan); err != nil {
			t.Fatal(err)
		}
	}
	if l, done, err := rsec.Reset(); err != nil || !l.Empty() {
		t.Fatalf("Reset = %+v, %+v, %v", l, done, err)
	}
	for _, d := range []string{QuietDir, NoStopDir} {
		if n := names(t, filepath.Join(rdir, d)); len(n) != 0 {
			t.Errorf("%s after a reset holds %v", d, n)
		}
	}
}
