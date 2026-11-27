//go:build unix

package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/wire"
)

// A start request is published without replacement, as a cancel request is: a second one for the
// same ticket finds it already requested. Reconciliation removes a request whose ticket is gone and
// keeps the live ticket's; removing the ticket removes it; a reset empties start/. Mutations:
// leaving StartDir out of noReplaceDirs replaces the second; out of reconciliation step 2 keeps the
// orphan; out of RemoveOwnTicket or the reset keeps the request.
func TestStartRequests(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	live, err := sec.Publish(QueueDir, ticketName(1), []byte(`{}`), true)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	idOf := func(seq int) wire.ID { return wire.ID{OrderKey: 1791720000000000000, PID: 4711, Seq: seq} }
	for _, seq := range []int{1, 2} {
		if _, err := sec.Publish(StartDir, idOf(seq).String(), []byte(`{}`), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sec.Publish(StartDir, idOf(1).String(), []byte(`{}`), false); !errors.Is(err, fs.ErrExist) {
		t.Errorf("a second start request: %v; want fs.ErrExist", err)
	}
	if removed, err := sec.ReconcileIfDue(time.Now().UnixNano()); err != nil || removed != 1 {
		t.Errorf("reconcile removed %d, %v; want 1, the orphaned request", removed, err)
	}
	path := func(seq int) string { return filepath.Join(dir, StartDir, idOf(seq).String()) }
	if _, err := os.Lstat(path(2)); !os.IsNotExist(err) {
		t.Errorf("the orphaned start request was kept: %v", err)
	}
	live.Close()
	if removed, err := sec.RemoveOwnTicket(ticketName(1)); err != nil || !removed {
		t.Fatalf("RemoveOwnTicket = %v, %v", removed, err)
	}
	if _, err := os.Lstat(path(1)); !os.IsNotExist(err) {
		t.Errorf("the start request outlived its ticket: %v", err)
	}
	sec.Leave()

	rdir := newDir(t)
	_, rsec := resetFixture(t, rdir)
	if _, err := rsec.Publish(StartDir, idOf(1).String(), []byte(`{}`), false); err != nil {
		t.Fatal(err)
	}
	if l, done, err := rsec.Reset(); err != nil || !l.Empty() {
		t.Fatalf("Reset = %+v, %+v, %v", l, done, err)
	}
	if names := names(t, filepath.Join(rdir, StartDir)); len(names) != 0 {
		t.Errorf("start/ after a reset holds %v", names)
	}
}
