//go:build unix

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/wire"
)

// The yield record is written and replaced inside the section and read with none; a file that is
// not a record reads as none; reconciliation removes the record once its head's ticket is gone and
// keeps a live head's; a reset removes it. Mutations: leaving the record out of reconciliation keeps
// the orphan; out of the reset keeps it.
func TestYieldRecord(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	live, err := sec.Publish(QueueDir, ticketName(1), []byte(`{}`), true)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	head := wire.ID{OrderKey: 1791720000000000000, PID: 4711, Seq: 1}.String()
	y := Yield{Head: head, StartNS: 10, UntilNS: 20}
	if err := sec.SetYield(y); err != nil {
		t.Fatal(err)
	}
	y.LastPassNS = 15
	if err := sec.SetYield(y); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.ReadYield(); err != nil || !ok || got != y {
		t.Errorf("ReadYield = %+v, %v, %v; want %+v", got, ok, err, y)
	}
	path := filepath.Join(dir, YieldFile)
	for _, bad := range []string{`{`, `{"head":"x","start_ns":1,"until_ns":2,"last_pass_ns":0}`, `{"head":"` + head + `","until_ns":-1}`} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.ReadYield(); ok || err != nil {
			t.Errorf("ReadYield of %s = %v, %v; want none", bad, ok, err)
		}
	}
	if err := sec.SetYield(y); err != nil {
		t.Fatal(err)
	}
	if removed, err := sec.ReconcileIfDue(time.Now().UnixNano()); err != nil || removed != 0 {
		t.Errorf("reconcile with the head live removed %d, %v; want 0", removed, err)
	}
	if err := sec.RemoveYield(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ReadYield(); ok {
		t.Error("the record outlived RemoveYield")
	}
	y.Head = wire.ID{OrderKey: 1791720000000000000, PID: 4711, Seq: 9}.String() // no such ticket
	if err := sec.SetYield(y); err != nil {
		t.Fatal(err)
	}
	if removed, err := sec.ReconcileIfDue(time.Now().Add(time.Minute).UnixNano()); err != nil || removed != 1 {
		t.Errorf("reconcile with the head gone removed %d, %v; want the record", removed, err)
	}
	sec.Leave()

	rdir := newDir(t)
	_, rsec := resetFixture(t, rdir)
	if err := rsec.SetYield(y); err != nil {
		t.Fatal(err)
	}
	if l, done, err := rsec.Reset(); err != nil || !l.Empty() {
		t.Fatalf("Reset = %+v, %+v, %v", l, done, err)
	}
	if _, err := os.Lstat(filepath.Join(rdir, YieldFile)); !os.IsNotExist(err) {
		t.Errorf("yield.json after a reset: %v", err)
	}
}
