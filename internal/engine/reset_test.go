//go:build unix

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/store"
)

// treeNames lists every path under dir, relative, for comparing before and after.
func treeNames(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// A reset with a live ticket, a live hold, a locked token or another client inside the section
// refuses with a *LiveError that names it, and changes nothing.
func TestResetRefusesLiveState(t *testing.T) {
	cases := []struct {
		name string
		live func(t *testing.T, e *Engine, dir string)
		want string
	}{
		{"ticket", func(t *testing.T, e *Engine, dir string) { startHelper(t, "enqueue", dir, "cpu=1") }, "1 live ticket (pid"},
		{"hold", func(t *testing.T, e *Engine, dir string) {
			g, _, err := e.Adopt(needLabel("cpu=2", "gate"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { g.Close() })
		}, "1 live hold (pid"},
		{"token", func(t *testing.T, e *Engine, dir string) { startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 2)) }, "locked tokens (cpu 3)"},
		{"section", func(t *testing.T, e *Engine, dir string) { startHelper(t, "section", dir, "") }, "another client is inside the admission section"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := newStateDir(t, "[resources]\ncpu = 4\n")
			e := newEngine(t, dir, nil)
			g, _, err := e.Adopt(need("cpu=1")) // FORMAT, gen and a hold exist
			if err != nil {
				t.Fatal(err)
			}
			g.Close()
			c.live(t, e, dir)
			before := treeNames(t, dir)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			removed, err := e.Reset(ctx)
			var le *LiveError
			if !errors.As(err, &le) || !errors.Is(err, ErrRefused) || removed != 0 {
				t.Fatalf("Reset = %d, %v; want a *LiveError matching ErrRefused", removed, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not say %q", err, c.want)
			}
			if after := treeNames(t, dir); after != before {
				t.Errorf("the refused reset changed the tree:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// With nothing alive a reset empties the state and leaves a directory a client can use again.
func TestResetThenUse(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 4\n")
	e := newEngine(t, dir, nil)
	g, _, err := e.Adopt(need("cpu=2"))
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	removed, err := e.Reset(context.Background())
	if err != nil || removed == 0 {
		t.Fatalf("Reset = %d, %v", removed, err)
	}
	for _, f := range []string{store.FormatFile, store.GenFile} {
		if exists(filepath.Join(dir, f)) {
			t.Errorf("%s survived", f)
		}
	}
	tk := enqueue(t, e, need("cpu=1"))
	if !isAdmitted(step(t, tk)) {
		t.Error("a request is not admitted after the reset")
	}
}
