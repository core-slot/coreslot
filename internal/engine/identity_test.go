//go:build unix

package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// Admission never reconciles (PROTOCOL.md section 10.4): a head admitted while reconciliation is
// due leaves a dead hold and gen's reconciled_ns as they were.
func TestAdmissionDoesNotReconcile(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	tk := enqueue(t, e, need(128)) // reconciles once, as enqueue does
	g, err := e.st.ReadGen()
	if err != nil {
		t.Fatal(err)
	}
	body, err := wire.EncodeGen(wire.Gen{Gen: g.N, ReconciledNS: 1}) // reconciliation is due
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, store.GenFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(dir, store.HoldsDir, wire.HoldName(1791720000000000000, 4711, 1, "dead"))
	if err := os.MkdirAll(filepath.Dir(dead), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dead, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := step(t, tk); !isAdmitted(r) {
		t.Fatalf("want admitted, got %#v", r)
	}
	if !exists(dead) {
		t.Error("the admission removed a dead hold: it reconciled")
	}
	if g, err := e.st.ReadGen(); err != nil || g.ReconciledNS != 1 {
		t.Errorf("gen after the admission: %+v, %v; want reconciled_ns 1 kept", g, err)
	}
}
