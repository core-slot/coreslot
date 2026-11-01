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

	"github.com/core-slot/coreslot/internal/store"
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

// A reset with a live ticket, a live hold or another client inside the section refuses with a
// *LiveError that names it, and changes nothing.
func TestResetRefusesLiveState(t *testing.T) {
	cases := []struct {
		name string
		live func(t *testing.T, e *Engine, dir string)
		want string
	}{
		{"ticket", func(t *testing.T, e *Engine, dir string) { startHelper(t, "enqueue", dir, "mem=1") }, "1 live ticket (pid"},
		{"hold", func(t *testing.T, e *Engine, dir string) {
			hold(t, e, needLabel("mem=2", "gate"))
		}, "1 live hold (pid"},
		{"another client's hold", func(t *testing.T, e *Engine, dir string) { startHelper(t, "lock", dir, holdPath(t, dir, 3)) }, "1 live hold (pid 4"},
		{"section", func(t *testing.T, e *Engine, dir string) { startHelper(t, "section", dir, "") }, "another client is inside the admission section"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := capped(t, 512)
			e := newEngine(t, dir, nil)
			hold(t, e, need("mem=1")).Close() // FORMAT, gen and a hold exist
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
	dir := capped(t, 512)
	e := newEngine(t, dir, nil)
	hold(t, e, needLabel("mem=1", "hold:three"))
	enqueue(t, e, needLabel("mem=1", "ci:o-r\x1b[2J"))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := e.Reset(ctx)
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
	dir := capped(t, 512)
	e := newEngine(t, dir, nil)
	hold(t, e, need("mem=1")).Close()
	setOtherLengthHolder(t, dir)
	sec, err := e.st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if live, _, err := sec.Reset(); err != nil || !live.Empty() {
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
	dir := capped(t, 512)
	e := newEngine(t, dir, nil)
	hold(t, e, need("mem=2")).Close()
	removed, err := e.Reset(context.Background())
	if err != nil || removed == 0 {
		t.Fatalf("Reset = %d, %v", removed, err)
	}
	if exists(filepath.Join(dir, store.GenFile)) {
		t.Error("gen survived")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, store.FormatFile)); string(b) != "coreslot-protocol 2\n" {
		t.Errorf("FORMAT after the reset %q, want coreslot-protocol 2", b)
	}
	tk := enqueue(t, e, need("mem=1"))
	if !isAdmitted(step(t, tk)) {
		t.Error("a request is not admitted after the reset")
	}
}

// A reset under a well-formed FORMAT this client does not implement recreates the directory as
// format 2, an entry of that format's queue included.
func TestResetRecreatesAForeignFormat(t *testing.T) {
	dir := capped(t, 512)
	e := newEngine(t, dir, nil)
	hold(t, e, need("mem=1")).Close()
	if err := os.WriteFile(filepath.Join(dir, store.FormatFile), []byte("coreslot-protocol 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, store.QueueDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, store.QueueDir, "v3-ticket"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reset(context.Background()); err != nil {
		t.Fatalf("Reset = %v; want the directory recreated", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, store.FormatFile)); string(b) != "coreslot-protocol 2\n" || exists(filepath.Join(dir, store.QueueDir, "v3-ticket")) {
		t.Errorf("after the reset FORMAT %q, v3-ticket present %v; want format 2 and an empty queue", b, exists(filepath.Join(dir, store.QueueDir, "v3-ticket")))
	}
}

// Snapshot over a store opened read-only creates nothing, on a missing and on an empty
// directory.
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
			e := New(st, filepath.Join(dir, store.ConfigFile),
				testMachine, SystemClock())
			rep, err := e.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if rep.StateDirExists != (name == "empty") || rep.Section.Held || len(rep.Holds)+len(rep.Tickets) != 0 {
				t.Errorf("report %+v", rep)
			}
			if rep.Mem.FreeMiB != 1024 {
				t.Errorf("mem %+v, want the whole room free", rep.Mem)
			}
			if after := treeContent(t, root); after != before {
				t.Errorf("Snapshot created entries:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}
