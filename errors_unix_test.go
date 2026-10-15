//go:build unix

package coreslot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// waitErr enqueues a cpu=1 request, runs between before Wait, and returns Wait's error.
func waitErr(t *testing.T, dir string, between func()) error {
	t.Helper()
	c := open(t, dir)
	w, err := c.Enqueue(context.Background(), Request{Need: map[string]Range{"cpu": {Min: 1, Max: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	between()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g, err := w.Wait(ctx)
	if err == nil {
		g.Close()
		t.Fatal("Wait admitted")
	}
	return err
}

// Every error the library returns is a *StateError unless it is one of the classes a caller acts
// on, and it wraps its cause: an I/O failure never reaches the caller raw (PROTOCOL.md section 14).
func TestIOErrorsAreStateErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Run("queue not writable", func(t *testing.T) {
		dir := stateDir(t, "")
		c := open(t, dir)
		q := filepath.Join(dir, "queue")
		if err := os.Mkdir(q, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(q, 0o700) })
		_, err := c.Enqueue(context.Background(), Request{})
		var se *StateError
		if !errors.As(err, &se) || !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("Enqueue into a read-only queue/: %v (%T), want a *StateError wrapping fs.ErrPermission", err, err)
		}
	})
	t.Run("state directory removed", func(t *testing.T) {
		dir := stateDir(t, "")
		err := waitErr(t, dir, func() {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		})
		var se *StateError
		if !errors.As(err, &se) || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Wait after the state directory was removed: %v (%T), want a *StateError wrapping fs.ErrNotExist", err, err)
		}
	})
	t.Run("state directory removed and recreated", func(t *testing.T) {
		dir := stateDir(t, "")
		err := waitErr(t, dir, func() {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		})
		var se *StateError
		if !errors.As(err, &se) {
			t.Fatalf("Wait after the state directory was recreated: %v (%T), want a *StateError", err, err)
		}
	})
}

// Open wraps the cause of a state directory it cannot create: a parent the user may not write
// is a *StateError that errors.Is still finds fs.ErrPermission in.
func TestOpenStateErrorWrapsItsCause(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := filepath.Join(t.TempDir(), "read-only")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o700) })
	_, err := Open(Options{StateDir: filepath.Join(parent, "state")})
	var se *StateError
	if !errors.As(err, &se) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Open under a read-only parent: %v (%T), want a *StateError wrapping fs.ErrPermission", err, err)
	}
}

// A relative Options.StateDir is the caller's mistake, a *UsageError: a *StateError would tell a
// caller that follows the contract to run every job unbudgeted.
func TestRelativeStateDirIsAUsageError(t *testing.T) {
	_, err := Open(Options{StateDir: "relative/state"})
	var (
		ue *UsageError
		se *StateError
	)
	if !errors.As(err, &ue) || errors.As(err, &se) {
		t.Fatalf("Open with a relative StateDir: %v (%T), want a *UsageError and no *StateError", err, err)
	}
}

// The classes a caller acts on are never state errors.
func TestDistinctErrorClasses(t *testing.T) {
	dir := stateDir(t, "")
	c := open(t, dir)
	var se *StateError
	if _, err := c.Available("gpu"); !errors.Is(err, ErrRefused) || errors.As(err, &se) {
		t.Errorf("Available of an undeclared resource: %v, want ErrRefused and no *StateError", err)
	}
	var ue *UsageError
	if _, err := c.Available("Not A Name"); !errors.As(err, &ue) {
		t.Errorf("Available of an invalid name: %v, want a *UsageError", err)
	}
	w, err := c.Enqueue(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.Wait(ctx); !errors.Is(err, context.Canceled) || errors.As(err, &se) {
		t.Errorf("Wait with an ended context: %v, want context.Canceled and no *StateError", err)
	}
	if err := w.Cancel(); err != nil {
		t.Errorf("Cancel after the abandoned Wait: %v", err)
	}
	for i := range 2 {
		if g, err := w.Poll(); g != nil || !errors.Is(err, ErrCanceled) {
			t.Errorf("Poll %d after the withdrawal: %v, %v; want ErrCanceled", i, g, err)
		}
	}
}

// holdSection takes the admission lock through another open file description, as a process
// stopped inside the section holds it, with admit.lock's body naming pid.
func holdSection(t *testing.T, dir string, pid int) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "admit.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(f, `{"pid":%d,"entered_ns":1}`, pid); err != nil {
		t.Fatal(err)
	}
}

// Enqueue and Adopt give up when ctx ends while the section is held: nothing is published, and
// the error is a *SectionBusyError that matches the deadline and names the holder's pid.
func TestBoundedEnqueueAndAdopt(t *testing.T) {
	dir := stateDir(t, "")
	c := open(t, dir)
	holdSection(t, dir, 424242)
	for _, op := range []struct {
		name string
		do   func(ctx context.Context) error
	}{
		{"Enqueue", func(ctx context.Context) error { _, err := c.Enqueue(ctx, Request{}); return err }},
		{"Adopt", func(ctx context.Context) error { _, err := c.Adopt(ctx, Request{}); return err }},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		start := time.Now()
		err := op.do(ctx)
		cancel()
		var (
			se   *StateError
			busy *SectionBusyError
		)
		if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &se) || !strings.Contains(err.Error(), "pid 424242") {
			t.Errorf("%s with the section held: %v; want the deadline, naming pid 424242, no *StateError", op.name, err)
		}
		if !errors.As(err, &busy) || busy.PID != 424242 {
			t.Errorf("%s with the section held: %v (%T), want a *SectionBusyError with PID 424242", op.name, err, err)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("%s returned after %v, want soon after the 300 ms deadline", op.name, took)
		}
	}
	r, err := c.Snapshot()
	if err != nil || len(r.Tickets) != 0 || len(r.Holds) != 0 {
		t.Errorf("snapshot: %d tickets, %d holds, %v; want nothing published", len(r.Tickets), len(r.Holds), err)
	}
}

// A grant whose state directory was removed and recreated under it reports a *StateError at Close
// and writes nothing into the new directory: no admit.lock, FORMAT or gen.
func TestCloseAfterTheStateDirectoryWasReplaced(t *testing.T) {
	dir := stateDir(t, "")
	c := open(t, dir)
	g := waitGrant(t, c, Request{Need: map[string]Range{"cpu": {Min: 3, Max: 3}}})
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var se *StateError
	if err := g.Close(); !errors.As(err, &se) {
		t.Fatalf("Close: %v, want a *StateError", err)
	}
	if names, err := os.ReadDir(dir); err != nil || len(names) != 0 {
		t.Errorf("the new state directory after Close: %v, %v; want it empty", names, err)
	}
}
