//go:build unix

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// onPoint runs fn once when the engine passes the pause point name, for the rest of the test.
func onPoint(t *testing.T, name string, fn func()) {
	t.Helper()
	old := point
	done := false
	point = func(n string) {
		if n == name && !done {
			done = true
			fn()
		}
	}
	t.Cleanup(func() { point = old })
}

// swapToken replaces res/cpu/0 with a new file, keeping the old inode reachable at a second
// name, which it returns, so that the test can probe whether anyone still locks it.
func swapToken(t *testing.T, dir string) (kept string) {
	t.Helper()
	tok := filepath.Join(dir, store.ResDir, "cpu", "0")
	kept = filepath.Join(t.TempDir(), "old-token")
	if err := os.Link(tok, kept); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tok+".new", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tok+".new", tok); err != nil {
		t.Fatal(err)
	}
	return kept
}

// A token file replaced between taking the tokens and the I1 check (PROTOCOL.md section 12.1
// step 6) fails admission, adoption and token-only admission alike: a state error, no hold
// published, and every token taken closed again.
func TestIdentityCheckAfterTakingTokens(t *testing.T) {
	cases := map[string]func(t *testing.T, e *Engine, dir string) error{
		"admission": func(t *testing.T, e *Engine, dir string) error {
			_, err := enqueue(t, e, need("cpu=2")).Step(t.Context())
			return err
		},
		"adoption": func(t *testing.T, e *Engine, dir string) error {
			_, err := e.AdoptContext(context.Background(), need("cpu=2"))
			return err
		},
		"token-only admission": func(t *testing.T, e *Engine, dir string) error {
			writeFormat(t, dir, "coreslot-protocol 99\n")
			_, err := enqueue(t, e, need("cpu=2")).Step(t.Context())
			return err
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			dir := newStateDir(t, "")
			e := newEngine(t, dir, newFakeClock())
			var kept string
			onPoint(t, "tokens.taken", func() { kept = swapToken(t, dir) })
			err := run(t, e, dir)
			var se *store.StateError
			if !errors.As(err, &se) {
				t.Fatalf("a token replaced before the I1 check: %v, want a state error", err)
			}
			if kept == "" {
				t.Fatal("the tokens.taken point was not passed")
			}
			if holds, _ := filepath.Glob(filepath.Join(dir, store.HoldsDir, "*")); len(holds) != 0 {
				t.Errorf("holds published: %v", holds)
			}
			if locked, err := store.ProbeLocked(kept); err != nil || locked {
				t.Errorf("the replaced token is still locked (%v, %v): the tokens taken were not closed", locked, err)
			}
			if n := lockedCount(t, e, "cpu"); n != 0 {
				t.Errorf("%d cpu tokens still locked, want 0", n)
			}
		})
	}
}

// A non-queued taker that locks a token after the plan and before the minimum pass (PROTOCOL.md
// section 12.1 step 6, I7) leaves the minimum short: the attempt closes what it took, publishes
// no hold, and the head keeps waiting.
func TestMinimumPassShortfallKeepsWaiting(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 2\n")
	e := newEngine(t, dir, newFakeClock())
	tk := enqueue(t, e, need("cpu=2"))
	var taker *os.File
	onPoint(t, "admit.planned", func() {
		p := filepath.Join(dir, store.ResDir, "cpu", "1")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		f, err := store.OpenFile(p, os.O_RDONLY|os.O_CREATE)
		if err != nil {
			t.Fatal(err)
		}
		if err := lockNB(f); err != nil {
			t.Fatal(err)
		}
		taker = f
	})
	r := step(t, tk)
	if taker == nil {
		t.Fatal("the admit.planned point was not passed")
	}
	defer taker.Close()
	if isAdmitted(r) {
		t.Fatalf("admitted with one of the two minimum tokens taken by another: %#v", r)
	}
	if holds, _ := filepath.Glob(filepath.Join(dir, store.HoldsDir, "*")); len(holds) != 0 {
		t.Errorf("holds published: %v", holds)
	}
	if n := lockedCount(t, e, "cpu"); n != 1 {
		t.Errorf("%d cpu tokens locked, want only the taker's 1 (the attempt's token closed again)", n)
	}
}

// Admission never reconciles (PROTOCOL.md section 10.4): a head admitted while reconciliation is
// due leaves a dead hold and gen's reconciled_ns as they were.
func TestAdmissionDoesNotReconcile(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	tk := enqueue(t, e, need("cpu=1")) // reconciles once, as enqueue does
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
