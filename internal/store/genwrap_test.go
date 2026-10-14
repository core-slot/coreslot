//go:build unix

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/wire"
)

const genMax = 1<<63 - 1

func writeGenBody(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, GenFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// PROTOCOL.md section 7.8: gen is in [0, 2^63-1]; a bump from the maximum writes 0, never a
// malformed (negative) value, and keeps reconciled_ns.
func TestBumpGenWrapsToZeroPastTheInt64Maximum(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	writeGenBody(t, dir, `{"gen":9223372036854775806,"reconciled_ns":1791719990000000000}`)
	if err := sec.BumpGen(); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.ReadGen(); g != (Gen{N: genMax, ReconciledNS: 1791719990000000000}) {
		t.Fatalf("bump from 2^63-2 = %+v, want gen 2^63-1", g)
	}
	if err := sec.BumpGen(); err != nil {
		t.Fatalf("bump from 2^63-1: %v", err)
	}
	if g, _ := s.ReadGen(); g != (Gen{N: 0, ReconciledNS: 1791719990000000000}) {
		t.Errorf("bump from 2^63-1 = %+v, want gen 0 with reconciled_ns unchanged", g)
	}
	body, err := os.ReadFile(filepath.Join(dir, GenFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wire.DecodeGen(body); err != nil {
		t.Errorf("the gen written past the maximum is malformed: %q: %v", body, err)
	}
}

// Reconciliation bumps gen when it removes something; it wraps the same way.
func TestReconcileWrapsGenToZeroPastTheInt64Maximum(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	f, err := sec.Publish(QueueDir, ticketName(1), []byte(`{}`), true)
	if err != nil {
		t.Fatal(err)
	}
	f.Close() // a dead ticket
	writeGenBody(t, dir, `{"gen":9223372036854775807,"reconciled_ns":1}`)
	now := time.Now().UnixNano()
	removed, err := sec.ReconcileIfDue(now)
	if err != nil || removed != 1 {
		t.Fatalf("ReconcileIfDue = %d, %v; want 1", removed, err)
	}
	if g, _ := s.ReadGen(); g != (Gen{N: 0, ReconciledNS: now}) {
		t.Errorf("gen after reconciling from 2^63-1 = %+v, want gen 0 and reconciled_ns %d", g, now)
	}
}
