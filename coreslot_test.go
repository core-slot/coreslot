//go:build unix

package coreslot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func stateDir(t *testing.T, cfg string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state dir") // a space, like Application Support
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func open(t *testing.T, dir string) *Client {
	t.Helper()
	c, err := Open(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func waitGrant(t *testing.T, c *Client, req Request) *Grant {
	t.Helper()
	w, err := c.Enqueue(req)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g, err := w.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

// The child of TestEnvMatchesFiles: it checks, from inside, that the descriptors the
// environment names are the grant's files.
const childEnv = "CORESLOT_ROOT_TEST_CHILD"

func TestMain(m *testing.M) {
	if dir := os.Getenv(childEnv); dir != "" {
		if err := checkChild(dir); err != nil {
			fmt.Fprintln(os.Stderr, "child:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// checkChild verifies the descriptor contract of PROTOCOL.md section 17 in the child: fd 3 is
// the hold (or, without CORESLOT_HELD_FD, neither a hold nor a token), and the fds of CORESLOT_TOKEN_FDS are the
// token files named in CORESLOT_ROOT_TEST_TOKENS, in order, each still locked.
func checkChild(dir string) error {
	var st unix.Stat_t
	if os.Getenv("CORESLOT_HELD_FD") == "3" {
		holds, _ := filepath.Glob(filepath.Join(dir, "holds", "0*"))
		if len(holds) != 1 {
			return fmt.Errorf("want one hold, have %v", holds)
		}
		if err := sameFile(3, holds[0]); err != nil {
			return err
		}
	} else if err := unix.Fstat(3, &st); err == nil {
		// fd 3 was closed at exec; the child's runtime may have reused it since (the race
		// runtime does on Linux), so require only that it is no hold and no token.
		files, _ := filepath.Glob(filepath.Join(dir, "holds", "*"))
		res, _ := filepath.Glob(filepath.Join(dir, "res", "*", "*"))
		for _, p := range append(files, res...) {
			if sameFile(3, p) == nil {
				return fmt.Errorf("fd 3 without a hold is %s", p)
			}
		}
	} else if !errors.Is(err, unix.EBADF) {
		return fmt.Errorf("fd 3: %v", err)
	}
	want := strings.Split(os.Getenv("CORESLOT_ROOT_TEST_TOKENS"), "\n")
	lo, hi, _ := strings.Cut(os.Getenv("CORESLOT_TOKEN_FDS"), "-")
	a, _ := strconv.Atoi(lo)
	b := a
	if hi != "" {
		b, _ = strconv.Atoi(hi)
	}
	if a != 4 || b-a+1 != len(want) {
		return fmt.Errorf("CORESLOT_TOKEN_FDS=%q for %d tokens", os.Getenv("CORESLOT_TOKEN_FDS"), len(want))
	}
	for i, p := range want {
		if err := sameFile(4+i, filepath.Join(dir, p)); err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(dir, p))
		if err != nil {
			return err
		}
		lerr := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		f.Close()
		if !errors.Is(lerr, unix.EWOULDBLOCK) {
			return fmt.Errorf("%s is not locked in the child: %v", p, lerr)
		}
	}
	return nil
}

func sameFile(fd int, path string) error {
	var a, b unix.Stat_t
	if err := unix.Fstat(fd, &a); err != nil {
		return fmt.Errorf("fd %d: %v", fd, err)
	}
	if err := unix.Stat(path, &b); err != nil {
		return err
	}
	if a.Dev != b.Dev || a.Ino != b.Ino {
		return fmt.Errorf("fd %d is not %s", fd, path)
	}
	return nil
}

func runChild(t *testing.T, dir string, g *Grant, tokens []string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.ExtraFiles = g.Files()
	cmd.Env = append(append(os.Environ(), g.Env()...),
		childEnv+"="+dir, "CORESLOT_ROOT_TEST_TOKENS="+strings.Join(tokens, "\n"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

func TestEnvMatchesFiles(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\npg-local = 2\n")
	c := open(t, dir)
	g := waitGrant(t, c, Request{Label: "gate", Need: map[string]Range{"cpu": {Min: 2, Max: 3}, "pg-local": {Min: 1, Max: 1}}})
	env := strings.Join(g.Env(), " ")
	if env != "CORESLOT_GRANTED_CPU=3 CORESLOT_GRANTED_PG_LOCAL=1 CORESLOT_HELD_FD=3 CORESLOT_TOKEN_FDS=4-7" {
		t.Errorf("env %q", env)
	}
	if files := g.Files(); len(files) != 5 || files[0] == nil {
		t.Fatalf("files %v, want the hold and 4 tokens", files)
	}
	runChild(t, dir, g, []string{"res/cpu/0", "res/cpu/1", "res/cpu/2", "res/pg-local/0"})

	// Token-only: no hold, fd 3 closed, tokens still from fd 4.
	g.Close()
	if err := os.WriteFile(filepath.Join(dir, "FORMAT"), []byte("coreslot-protocol 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	to := waitGrant(t, c, Request{Need: map[string]Range{"cpu": {Min: 2, Max: 3}}})
	if !to.TokenOnly() || to.Files()[0] != nil {
		t.Fatalf("token-only %v files %v", to.TokenOnly(), to.Files())
	}
	if env := strings.Join(to.Env(), " "); env != "CORESLOT_GRANTED_CPU=2 CORESLOT_TOKEN_FDS=4-5 CORESLOT_UNBUDGETED=format" {
		t.Errorf("token-only env %q", env)
	}
	runChild(t, dir, to, []string{"res/cpu/0", "res/cpu/1"})
}

// The Waiter's concurrency contract: the first Poll or Wait to admit wins and the others return the
// same grant; Cancel after admission is ErrAlreadyAdmitted; Cancel before it makes Wait return
// ErrCancelled.
func TestConcurrentPollWaitCancel(t *testing.T) {
	dir := stateDir(t, "")
	c := open(t, dir)
	sawAdmitted, sawCancelled := false, false
	for i := 0; i < 30; i++ {
		w, err := c.Enqueue(Request{Need: map[string]Range{"cpu": {Min: 1, Max: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var grants []*Grant
		var errs []error
		record := func(g *Grant, err error) {
			mu.Lock()
			defer mu.Unlock()
			if g != nil {
				grants = append(grants, g)
			}
			if err != nil {
				errs = append(errs, err)
			}
		}
		var wg sync.WaitGroup
		wg.Add(4)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			record(w.Wait(ctx))
		}()
		for p := 0; p < 2; p++ {
			go func() {
				defer wg.Done()
				for k := 0; k < 5; k++ {
					g, ok, err := w.Poll()
					if ok != (g != nil) {
						t.Errorf("Poll: ok %v with grant %v", ok, g)
					}
					record(g, err)
				}
			}()
		}
		var cancelErr error
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i%6) * time.Millisecond)
			cancelErr = w.Cancel()
		}()
		wg.Wait()

		switch {
		case cancelErr == nil:
			sawCancelled = true
			if len(grants) != 0 {
				t.Fatalf("iteration %d: Cancel withdrew, yet a grant was returned", i)
			}
			for _, err := range errs {
				if !errors.Is(err, ErrCancelled) {
					t.Fatalf("iteration %d: after Cancel: %v, want ErrCancelled", i, err)
				}
			}
			if len(errs) == 0 {
				t.Fatalf("iteration %d: Wait returned neither a grant nor ErrCancelled", i)
			}
		case errors.Is(cancelErr, ErrAlreadyAdmitted):
			sawAdmitted = true
			if len(grants) == 0 || len(errs) != 0 {
				t.Fatalf("iteration %d: admitted, but grants %v errors %v", i, grants, errs)
			}
			for _, g := range grants {
				if g != grants[0] {
					t.Fatalf("iteration %d: two different grants returned", i)
				}
			}
			grants[0].Close()
		default:
			t.Fatalf("iteration %d: Cancel: %v", i, cancelErr)
		}
		if err := w.Cancel(); err != nil && !errors.Is(err, ErrAlreadyAdmitted) {
			t.Fatalf("iteration %d: a second Cancel: %v", i, err)
		}
	}
	t.Logf("outcomes seen: admitted %v, cancelled %v", sawAdmitted, sawCancelled)
}

// PROTOCOL.md section 5.3: Options.StateDir moves the configuration to <StateDir>/config.toml,
// which must pass the state directory's policy.
func TestStateDirOptionMovesConfiguration(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 3\n")
	c := open(t, dir)
	if n, err := c.Free("cpu"); err != nil || n != 3 {
		t.Fatalf("Free = %d, %v; want the state directory's configuration (3)", n, err)
	}
	cfg := filepath.Join(dir, "config.toml")
	if err := os.Chmod(cfg, 0o620); err != nil {
		t.Fatal(err)
	}
	var se *StateError
	if _, err := c.Free("cpu"); !errors.As(err, &se) {
		t.Errorf("group-writable config.toml: %v, want a *StateError", err)
	}
	if _, err := Open(Options{StateDir: dir}); !errors.As(err, &se) {
		t.Errorf("Open with a group-writable config.toml: %v, want a *StateError", err)
	}
	if _, err := Open(Options{StateDir: "relative/dir"}); !errors.As(err, &se) {
		t.Errorf("relative StateDir: %v, want a *StateError", err)
	}
}

func TestUsageErrorBeforeQueueing(t *testing.T) {
	c := open(t, stateDir(t, ""))
	var ue *UsageError
	if _, err := c.Enqueue(Request{Need: map[string]Range{"cpu": {Min: 3, Max: 2}}}); !errors.As(err, &ue) {
		t.Errorf("min > max: %v, want a *UsageError", err)
	}
	if r, err := c.Snapshot(); err != nil || len(r.Tickets) != 0 {
		t.Errorf("snapshot %+v, %v: want no ticket", r.Tickets, err)
	}
}

// A Wait that fails withdraws its ticket: it is dead at once to every other client instead of
// blocking the queue until it stalls.
func TestWaitWithdrawsOnStateError(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 0\n") // invalid: read only at admission
	c := open(t, dir)
	w, err := c.Enqueue(Request{Need: map[string]Range{"cpu": {Min: 1, Max: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Snapshot()
	if err != nil || len(r.Tickets) != 1 || r.Tickets[0].Dead {
		t.Fatalf("before Wait: tickets %+v, %v; want one live ticket", r.Tickets, err)
	}
	queued := filepath.Join(dir, "queue", r.Tickets[0].Name)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var se *StateError
	if _, err := w.Wait(ctx); !errors.As(err, &se) {
		t.Fatalf("Wait: %v, want a *StateError", err)
	}
	if f, err := os.Open(queued); err == nil {
		lerr := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		f.Close()
		if lerr != nil {
			t.Fatalf("after the failed Wait the ticket is still locked (live): %v", lerr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := w.Cancel(); err != nil {
		t.Errorf("Cancel after the failed Wait: %v, want nil (already withdrawn)", err)
	}
}
