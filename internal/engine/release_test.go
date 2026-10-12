//go:build unix

package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Nezhinskiy/coreslot/internal/store"
)

// replaceDir removes the state directory under a holder and, when recreate, makes an empty one in
// its place.
func replaceDir(t *testing.T, dir string, recreate bool) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if recreate {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// A holder whose state directory was removed, or removed and recreated, under it finds out when
// it releases (I1): Close reports a state error and writes nothing into the directory as it now
// exists, neither a hold removal nor a section entry (which would create admit.lock and FORMAT).
func TestReleaseAfterTheStateDirectoryWasReplaced(t *testing.T) {
	grants := map[string]func(t *testing.T, e *Engine) *Grant{
		"admitted": func(t *testing.T, e *Engine) *Grant {
			return admitted(t, step(t, enqueue(t, e, need("cpu=3"))))
		},
		"adopted": func(t *testing.T, e *Engine) *Grant {
			g, _, err := e.Adopt(need("cpu=2"))
			if err != nil {
				t.Fatal(err)
			}
			return g
		},
		"fail-open": func(t *testing.T, e *Engine) *Grant {
			g, err := e.FailOpen(need("cpu=2"), "test")
			if err != nil {
				t.Fatal(err)
			}
			return g
		},
	}
	for name, grant := range grants {
		for _, recreate := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				dir := newStateDir(t, "")
				e := newEngine(t, dir, nil)
				g := grant(t, e)
				replaceDir(t, dir, recreate)
				before := listing(t, filepath.Dir(dir))
				err := g.Close()
				var se *store.StateError
				var mismatch *IdentityError
				if !errors.As(err, &se) || !errors.As(err, &mismatch) {
					t.Fatalf("Close after the state directory was replaced (recreated %v): %v, want a state error that is an *IdentityError", recreate, err)
				}
				if after := listing(t, filepath.Dir(dir)); after != before {
					t.Errorf("Close wrote into the replaced state directory:\n%s\n->\n%s", before, after)
				}
				if err := g.Close(); err != nil {
					t.Errorf("second Close: %v, want nil", err)
				}
			})
		}
	}
}

// One token file replaced under the holder: Close reports it and leaves the hold in place, dead,
// for reconciliation; it never removes a hold by a path whose identity it no longer trusts.
func TestReleaseAfterATokenWasReplaced(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, nil)
	g := admitted(t, step(t, enqueue(t, e, need("cpu=2"))))
	hold := filepath.Join(dir, store.HoldsDir, g.HoldName())
	tok := filepath.Join(dir, store.ResDir, "cpu", "1")
	if err := os.WriteFile(tok+".new", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tok+".new", tok); err != nil {
		t.Fatal(err)
	}
	var se *store.StateError
	var mismatch *IdentityError
	if err := g.Close(); !errors.As(err, &se) || !errors.As(err, &mismatch) {
		t.Fatalf("Close after a token was replaced: %v, want a state error that is an *IdentityError", err)
	}
	if !exists(hold) {
		t.Error("the hold was removed after the identity check failed")
	}
	if dead, err := e.st.Probe(store.HoldsDir, g.HoldName()); err != nil || !dead {
		t.Errorf("the hold after Close: dead %v, %v; want dead (its descriptor closed)", dead, err)
	}
}

// Without a replacement, Close removes the hold as before.
func TestReleaseRemovesTheHold(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, nil)
	g := admitted(t, step(t, enqueue(t, e, need("cpu=2"))))
	hold := filepath.Join(dir, store.HoldsDir, g.HoldName())
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if exists(hold) {
		t.Error("the hold is still there after Close")
	}
}
