//go:build unix

package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/store"
)

// treeContent is every path under dir, relative, with its mode, size and inode, and the bytes
// of every regular file, for comparing before and after. Names alone would not see a refused
// reset rewrite admit.lock, and a size alone would pass or fail with the length of a pid.
func treeContent(t *testing.T, dir string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(&out, "%s %s size=%d ino=%d", rel, fi.Mode(), fi.Size(), fi.Sys().(*syscall.Stat_t).Ino)
		if fi.Mode().IsRegular() {
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&out, " body=%q", body)
		}
		out.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// setOtherLengthHolder rewrites admit.lock's body, in place, as a record of a pid with a
// different number of digits than this process's: a reset that marks itself as holder then
// changes the file's size as well as its content, whatever pid this run drew.
func setOtherLengthHolder(t *testing.T, dir string) {
	t.Helper()
	pid := strings.Repeat("9", len(fmt.Sprint(os.Getpid()))+1)
	body := fmt.Sprintf(`{"pid":%s,"entered_ns":1}`, pid)
	if err := os.WriteFile(filepath.Join(dir, store.AdmitLock), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
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
			setOtherLengthHolder(t, dir)
			before := treeContent(t, dir)
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
			if after := treeContent(t, dir); after != before {
				t.Errorf("the refused reset changed the tree:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// A refusal names each live hold and ticket by its label, escaped for a terminal, not by the
// display slug of its file name.
func TestResetRefusalNamesLabels(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 4\n")
	e := newEngine(t, dir, nil)
	g, _, err := e.Adopt(needLabel("cpu=1", "hold:three"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	enqueue(t, e, needLabel("cpu=1", "ci:o-r\x1b[2J"))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = e.Reset(ctx)
	var le *LiveError
	if !errors.As(err, &le) {
		t.Fatalf("Reset = %v, want a *LiveError", err)
	}
	pid := os.Getpid()
	for _, want := range []string{
		fmt.Sprintf("1 live hold (pid %d, hold:three)", pid),
		fmt.Sprintf("1 live ticket (pid %d, ci:o-r\\x1b[2J)", pid),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not say %q", err, want)
		}
	}
}

// While an accepted reset works it is the section's recorded holder: status reads admit.lock's
// body, outside the section, and must name this process (not a stale record, whatever length
// its pid had) with a fresh entry time. The section is kept after Reset returns, as it is while
// the removals run.
func TestAcceptedResetNamesItselfHolder(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 4\n")
	e := newEngine(t, dir, nil)
	g, _, err := e.Adopt(need("cpu=1"))
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	setOtherLengthHolder(t, dir)
	sec, err := e.st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if live, _, err := sec.Reset(formats()); err != nil || !live.Empty() {
		t.Fatalf("Reset = %+v, %v", live, err)
	}
	rep, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Section.Held || rep.Section.PID != os.Getpid() || rep.Section.EnteredNS <= 1 {
		t.Errorf("status sees section %+v; want held by pid %d with a fresh entry time", rep.Section, os.Getpid())
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

// A reset under a well-formed FORMAT this client does not implement refuses and names it.
func TestResetRefusesForeignFormat(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 4\n")
	e := newEngine(t, dir, nil)
	g, _, err := e.Adopt(need("cpu=1"))
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	if err := os.WriteFile(filepath.Join(dir, store.FormatFile), []byte("coreslot-protocol 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, store.QueueDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, store.QueueDir, "v2-ticket"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	setOtherLengthHolder(t, dir)
	before := treeContent(t, dir)
	_, err = e.Reset(context.Background())
	var le *LiveError
	if !errors.As(err, &le) || !errors.Is(err, ErrRefused) ||
		!strings.Contains(err.Error(), "format 2 is not implemented by this client; use a coreslot that implements it") {
		t.Fatalf("Reset = %v; want a refusal naming format 2", err)
	}
	if after := treeContent(t, dir); after != before {
		t.Errorf("the refused reset changed the tree:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// Snapshot over a store opened read-only creates nothing, on a missing and on an empty
// directory, and reports locked tokens of resources the configuration does not declare.
func TestSnapshotReadOnlyCreatesNothing(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"missing": missing, "empty": empty} {
		t.Run(name, func(t *testing.T) {
			before := treeContent(t, root)
			st, err := store.OpenReadOnly(dir)
			if err != nil {
				t.Fatal(err)
			}
			e := New(st, &config.Watcher{Path: filepath.Join(dir, store.ConfigFile)},
				func() (int, error) { return testCPUs, nil }, SystemClock())
			rep, err := e.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if rep.StateDirExists != (name == "empty") || rep.Section.Held || len(rep.Holds)+len(rep.Tickets) != 0 {
				t.Errorf("report %+v", rep)
			}
			if rep.Resources["cpu"].Size != testCPUs {
				t.Errorf("resources %v, want the default cpu budget", rep.Resources)
			}
			if after := treeContent(t, root); after != before {
				t.Errorf("Snapshot created entries:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestSnapshotReportsUndeclaredLockedTokens(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 4\n")
	e := newEngine(t, dir, nil)
	startHelper(t, "lock", dir, tokenPaths(dir, "old-gpu", 0, 1))
	rep, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if rep.UndeclaredLocked["old-gpu"] != 2 || len(rep.UndeclaredLocked) != 1 {
		t.Errorf("UndeclaredLocked = %v, want old-gpu 2 only", rep.UndeclaredLocked)
	}
	if _, ok := rep.Resources["old-gpu"]; ok {
		t.Errorf("an undeclared resource is listed as declared: %v", rep.Resources)
	}
}
