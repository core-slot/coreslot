//go:build unix

package coreslot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The library's pass-through (PROTOCOL.md section 18, EnqueueContext). The cases that need a
// real inherited grant run in a child of the test binary started with the grant's descriptors at
// numbers of the test's choosing (nestedEnv), since a grant is inherited, not made in-process.

// nestedEnv selects a pass-through helper mode of the test binary: "child" is the process inside
// a grant, "grandchild" the command it starts with the pass-through's Files and Env.
const nestedEnv = "CORESLOT_ROOT_TEST_NESTED"

// tree lists every entry under dir with its size and modification time, so that two listings
// differ when anything was created, removed or written.
func tree(t testing.TB, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %d %d\n", p, fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// withoutCoreslot is the environment without any CORESLOT_* variable: the test may itself run
// inside a coreslot run, whose context is not the test's.
func withoutCoreslot() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "CORESLOT_") })
}

// clearContext unsets every CORESLOT_* variable for the test, restoring them after it.
func clearContext(t *testing.T) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "CORESLOT_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
}

// Rule 1: under a parent that runs unbudgeted the grant comes at once, every requested resource
// unbudgeted with its Min, no descriptors, and the inherited context passed on with the requested
// CORESLOT_GRANTED_<RES> set to the Min; nothing is written. Options.Independent queues instead,
// and a Client on another state directory is another budget. Mutation: making EnqueueContext
// ignore the inherited context reddens "pass-through"; dropping the budget check of
// DecideNested reddens "another budget".
func TestPassThroughUnderUnbudgetedParent(t *testing.T) {
	clearContext(t)
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	t.Setenv("CORESLOT_STATE_DIR", dir)
	t.Setenv("CORESLOT_UNBUDGETED", "parent-reason")
	t.Setenv("CORESLOT_GRANTED_CPU", "5")
	t.Setenv("CORESLOT_GRANTED_GPU", "7")
	req := Request{Need: map[string]Range{"cpu": {Min: 3, Max: 6}}}

	t.Run("pass-through", func(t *testing.T) {
		c := open(t, dir)
		before := tree(t, dir)
		w, err := c.Enqueue(req)
		if err != nil {
			t.Fatal(err)
		}
		g, ok, err := w.Poll()
		if !ok || err != nil {
			t.Fatalf("Poll = %v, %v, %v; want the grant at once", g, ok, err)
		}
		if got, _ := w.Wait(context.Background()); got != g {
			t.Errorf("Wait returned another grant")
		}
		if err := w.Cancel(); !errors.Is(err, ErrAlreadyAdmitted) {
			t.Errorf("Cancel = %v, want ErrAlreadyAdmitted", err)
		}
		if !g.PassThrough() || !maps.Equal(g.Grant(), map[string]int{"cpu": 3}) ||
			!slices.Equal(g.Unbudgeted(), []string{"cpu"}) || len(g.Clamped()) != 0 || g.Files() != nil {
			t.Errorf("grant: pass-through %v, %v, unbudgeted %v, clamped %v, files %v", g.PassThrough(), g.Grant(), g.Unbudgeted(), g.Clamped(), g.Files())
		}
		want := "CORESLOT_GRANTED_GPU=7 CORESLOT_UNBUDGETED=parent-reason CORESLOT_GRANTED_CPU=3"
		if env := strings.Join(g.Env(), " "); env != want {
			t.Errorf("env %q, want %q", env, want)
		}
		if err := g.Close(); err != nil {
			t.Error(err)
		}
		if after := tree(t, dir); after != before {
			t.Errorf("the pass-through wrote under the state directory:\n%s\n---\n%s", before, after)
		}
	})
	t.Run("independent", func(t *testing.T) {
		c, err := Open(Options{StateDir: dir, Independent: true})
		if err != nil {
			t.Fatal(err)
		}
		g := waitGrant(t, c, req)
		if g.PassThrough() || g.Grant()["cpu"] != 4 || strings.Contains(strings.Join(g.Env(), " "), "UNBUDGETED") {
			t.Errorf("an Independent request: pass-through %v, grant %v, env %v; want an ordinary grant", g.PassThrough(), g.Grant(), g.Env())
		}
	})
	t.Run("another budget", func(t *testing.T) {
		g := waitGrant(t, open(t, stateDir(t, "[resources]\ncpu = 4\n")), req)
		if g.PassThrough() || g.Grant()["cpu"] != 4 {
			t.Errorf("a Client on another state directory: pass-through %v, grant %v; want an ordinary grant", g.PassThrough(), g.Grant())
		}
	})
}

// A Client opened on Options.StateDir under a context whose state directory cannot be resolved
// (here a CORESLOT_STATE_DIR that is not an absolute path; a home-directory lookup that fails is
// the other way) cannot tell whether the context is its budget's: it refuses, at once and without
// writing anything, rather than queue, since Options.StateDir may spell out the very directory the
// context names, and the request would then queue behind its own parent. Options.Independent
// queues. Mutation: taking a context whose directory cannot be resolved for another budget's,
// as an ordinary request, reddens it (an ordinary grant instead of the refusal).
func TestPassThroughRefusesAContextOfUnknownDirectory(t *testing.T) {
	clearContext(t)
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	t.Setenv("CORESLOT_STATE_DIR", "relative/state")
	t.Setenv("CORESLOT_UNBUDGETED", "parent-reason")
	req := Request{Need: map[string]Range{"cpu": {Min: 1, Max: 1}}}
	c := open(t, dir)
	before := tree(t, dir)
	w, err := c.Enqueue(req)
	var se *StateError
	if w != nil || !errors.Is(err, ErrRefused) || errors.As(err, &se) {
		if w != nil {
			if g, ok, _ := w.Poll(); ok {
				defer g.Close()
			} else {
				_ = w.Cancel()
			}
		}
		t.Fatalf("Enqueue = %v, %v; want an error matching ErrRefused, not a *StateError", w, err)
	}
	if !strings.Contains(err.Error(), "cannot be resolved") || !strings.Contains(err.Error(), "relative/state") ||
		!strings.Contains(err.Error(), "Options.Independent") {
		t.Errorf("error %q: want the cause and the way out", err)
	}
	if after := tree(t, dir); after != before {
		t.Errorf("the refusal wrote under the state directory:\n%s\n---\n%s", before, after)
	}
	c, err = Open(Options{StateDir: dir, Independent: true})
	if err != nil {
		t.Fatal(err)
	}
	if g := waitGrant(t, c, req); g.PassThrough() {
		t.Error("an Independent request passed through")
	}
}

// A context that does not validate is a refusal, at once and without writing anything: the
// request never queues behind a parent that may hold the whole budget. Mutation: queueing an
// ordinary request when the context does not validate reddens it (no error).
func TestPassThroughRefusesAnInvalidContext(t *testing.T) {
	clearContext(t)
	dir := stateDir(t, "")
	closed := 1000
	for _, err := unix.FcntlInt(uintptr(closed), unix.F_GETFD, 0); err == nil; _, err = unix.FcntlInt(uintptr(closed), unix.F_GETFD, 0) {
		closed++
	}
	t.Setenv("CORESLOT_STATE_DIR", dir)
	t.Setenv("CORESLOT_TOKEN_FDS", strconv.Itoa(closed))
	c := open(t, dir)
	before := tree(t, dir)
	start := time.Now()
	w, err := c.Enqueue(Request{})
	took := time.Since(start)
	var se *StateError
	if w != nil || !errors.Is(err, ErrRefused) || errors.As(err, &se) {
		t.Fatalf("Enqueue = %v, %v; want an error matching ErrRefused, not a *StateError", w, err)
	}
	if !strings.Contains(err.Error(), "descriptor "+strconv.Itoa(closed)+" of CORESLOT_TOKEN_FDS is not open") ||
		!strings.Contains(err.Error(), "Options.Independent") {
		t.Errorf("error %q: want the diagnostic and the way out", err)
	}
	if took > time.Second {
		t.Errorf("refused after %v, want at once", took)
	}
	if after := tree(t, dir); after != before {
		t.Errorf("the refusal wrote under the state directory:\n%s\n---\n%s", before, after)
	}
	c, err = Open(Options{StateDir: dir, Independent: true})
	if err != nil {
		t.Fatal(err)
	}
	if g := waitGrant(t, c, Request{}); g.PassThrough() {
		t.Error("an Independent request under an invalid context passed through")
	}
}

// Rule 2 with a real inherited grant: the test takes a grant of cpu 0-2 and pg-local 0 and starts
// the child with the hold at fd 6 and the tokens at fds 9 to 12 in another order (pg-local/0,
// cpu/2, cpu/0, cpu/1), so that every number the child's command sees was renumbered. The child
// (passThroughChild) checks the grant, starts the grandchild with Files and Env, and checks that
// Close and a collection leave the inherited descriptors open for a second pass-through.
// Mutation: an Env that names the inherited numbers (6 and 9-12) instead of the ones Files
// places the descriptors at reddens it in the grandchild; a Close that closes the descriptors
// reddens it after the Close; wrapping each descriptor in a new *os.File at every validation
// reddens it after the collection, which closes the first wrappers' descriptors.
func TestPassThroughOnInheritedGrant(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\npg-local = 2\n")
	g := waitGrant(t, open(t, dir), Request{Label: "parent",
		Need: map[string]Range{"cpu": {Min: 3, Max: 3}, "pg-local": {Min: 1, Max: 1}}})
	f := g.Files() // hold, cpu/0, cpu/1, cpu/2, pg-local/0
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.ExtraFiles = []*os.File{nil, nil, nil, f[0], nil, nil, f[4], f[3], f[1], f[2]}
	cmd.Env = append(withoutCoreslot(), nestedEnv+"=child", "CORESLOT_STATE_DIR="+dir,
		"CORESLOT_HELD_FD=6", "CORESLOT_TOKEN_FDS=9-12",
		"CORESLOT_GRANTED_CPU=3", "CORESLOT_GRANTED_PG_LOCAL=1", "CORESLOT_GRANTED_OTHER=9")
	out, err := cmd.CombinedOutput()
	t.Logf("child:\n%s", out)
	if err != nil {
		t.Fatalf("child: %v", err)
	}
}

// moreHelpers are the helper modes of nestedEnv that test files of other build tags add.
var moreHelpers = map[string]func() error{}

// passThroughHelper runs a helper mode of nestedEnv and returns its exit status.
func passThroughHelper(mode string) int {
	var err error
	switch mode {
	case "child":
		err = passThroughChild()
	case "grandchild":
		err = passThroughGrandchild()
	default:
		if h := moreHelpers[mode]; h != nil {
			err = h()
		} else {
			err = fmt.Errorf("unknown mode %q", mode)
		}
	}
	if err != nil {
		fmt.Println(mode+":", err)
		return 1
	}
	return 0
}

// passThroughChild is the process inside the grant of TestPassThroughOnInheritedGrant.
func passThroughChild() error {
	dir := os.Getenv("CORESLOT_STATE_DIR")
	c, err := Open(Options{})
	if err != nil {
		return err
	}
	before := tree(nopTB{}, dir)
	req := Request{Need: map[string]Range{"cpu": {Min: 1, Max: 2}, "pg-local": {Min: 2, Max: 2}, "gpu": {Min: 1, Max: 1}}}
	enqueue := func() (*Grant, error) {
		w, err := c.Enqueue(req)
		if err != nil {
			return nil, err
		}
		// A pass-through needs no time at all; the bound only ends a request that queued.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return w.Wait(ctx)
	}
	g, err := enqueue()
	if err != nil {
		return err
	}
	if !g.PassThrough() {
		return fmt.Errorf("not a pass-through")
	}
	if want := map[string]int{"cpu": 2, "pg-local": 1, "gpu": 1}; !maps.Equal(g.Grant(), want) {
		return fmt.Errorf("grant %v, want %v: min(max, held), the clamp, and gpu unbudgeted with its min", g.Grant(), want)
	}
	if want := map[string]int{"pg-local": 2}; !maps.Equal(g.Clamped(), want) {
		return fmt.Errorf("clamped %v, want %v", g.Clamped(), want)
	}
	if !slices.Equal(g.Unbudgeted(), []string{"gpu"}) {
		return fmt.Errorf("unbudgeted %v, want [gpu]", g.Unbudgeted())
	}
	// The parent's every token, in ascending resource name and index, behind the parent's hold.
	var fds []uintptr
	for _, f := range g.Files() {
		fds = append(fds, f.Fd())
	}
	if want := []uintptr{6, 11, 12, 10, 9}; !slices.Equal(fds, want) {
		return fmt.Errorf("Files are fds %v, want %v", fds, want)
	}

	// The command sees, at the numbers Env names, the parent's hold and tokens.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.ExtraFiles = g.Files()
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return strings.HasPrefix(k, "CORESLOT_GRANTED_") || k == "CORESLOT_HELD_FD" ||
			k == "CORESLOT_TOKEN_FDS" || k == "CORESLOT_UNBUDGETED" || k == nestedEnv
	}), append(g.Env(), nestedEnv+"=grandchild")...)
	out, err := cmd.CombinedOutput()
	fmt.Printf("grandchild:\n%s", out)
	if err != nil {
		return fmt.Errorf("grandchild: %v", err)
	}
	// The inherited CORESLOT_GRANTED_OTHER, which the request does not name, is passed on
	// unchanged, as a nested `coreslot run` passes it.
	env := strings.Join(g.Env(), " ")
	if want := "CORESLOT_GRANTED_OTHER=9 CORESLOT_GRANTED_CPU=2 CORESLOT_GRANTED_GPU=1 CORESLOT_GRANTED_PG_LOCAL=1 " +
		"CORESLOT_HELD_FD=3 CORESLOT_TOKEN_FDS=4-7"; env != want {
		return fmt.Errorf("env %q, want %q", env, want)
	}

	// Close closes none of the inherited descriptors, and no collection of a wrapper does: later
	// pass-throughs still validate. Each round drops the grant, collects (finalizers run on their
	// own goroutine, hence the pauses) and checks the descriptors; a validation that wrapped
	// them anew would leave the previous wrappers to a later round's collection.
	for round := 1; round <= 3; round++ {
		if err := g.Close(); err != nil {
			return err
		}
		g = nil
		for range 3 {
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
		}
		for _, fd := range []int{6, 9, 10, 11, 12} {
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil {
				return fmt.Errorf("round %d: after Close and a collection, inherited fd %d: %v", round, fd, err)
			}
		}
		if g, err = enqueue(); err != nil {
			return fmt.Errorf("round %d: a pass-through after Close and a collection: %v", round, err)
		}
		if !g.PassThrough() || g.Grant()["cpu"] != 2 || g.Files()[1].Fd() != 11 {
			return fmt.Errorf("round %d: the pass-through's grant %v, files %v", round, g.Grant(), g.Files())
		}
	}
	if after := tree(nopTB{}, dir); after != before {
		return fmt.Errorf("the pass-throughs wrote under the state directory:\n%s\n---\n%s", before, after)
	}
	fmt.Println("child: ok")
	return nil
}

// passThroughGrandchild is the command the child starts: the descriptors CORESLOT_HELD_FD and
// CORESLOT_TOKEN_FDS name must be the parent's hold and its tokens, in order, still locked; and a
// pass-through of its own counts every token the child passed on, not the child's narrowed grant.
func passThroughGrandchild() error {
	dir := os.Getenv("CORESLOT_STATE_DIR")
	holds, _ := filepath.Glob(filepath.Join(dir, "holds", "0*"))
	if len(holds) != 1 {
		return fmt.Errorf("want one hold, the parent's; have %v", holds)
	}
	held, err := strconv.Atoi(os.Getenv("CORESLOT_HELD_FD"))
	if err != nil {
		return fmt.Errorf("CORESLOT_HELD_FD: %v", err)
	}
	if err := sameFile(held, holds[0]); err != nil {
		return fmt.Errorf("CORESLOT_HELD_FD=%d: %v", held, err)
	}
	lo, hi, _ := strings.Cut(os.Getenv("CORESLOT_TOKEN_FDS"), "-")
	a, _ := strconv.Atoi(lo)
	b, _ := strconv.Atoi(hi)
	tokens := []string{"res/cpu/0", "res/cpu/1", "res/cpu/2", "res/pg-local/0"}
	if b-a+1 != len(tokens) {
		return fmt.Errorf("CORESLOT_TOKEN_FDS=%q for %d tokens", os.Getenv("CORESLOT_TOKEN_FDS"), len(tokens))
	}
	for i, p := range tokens {
		if err := sameFile(a+i, filepath.Join(dir, p)); err != nil {
			return fmt.Errorf("CORESLOT_TOKEN_FDS=%s: %v", os.Getenv("CORESLOT_TOKEN_FDS"), err)
		}
	}
	c, err := Open(Options{})
	if err != nil {
		return err
	}
	w, err := c.Enqueue(Request{Need: map[string]Range{"cpu": {Min: 1, Max: 8}}})
	if err != nil {
		return err
	}
	g, err := w.Wait(context.Background())
	if err != nil {
		return err
	}
	if !g.PassThrough() || g.Grant()["cpu"] != 3 {
		return fmt.Errorf("the grandchild's pass-through: %v, want cpu 3, every token the child passed on", g.Grant())
	}
	fmt.Println("grandchild: ok")
	return nil
}

// nopTB lets tree run in a helper process, where there is no *testing.T: a failure panics.
type nopTB struct{ testing.TB }

func (nopTB) Helper() {}

func (nopTB) Fatal(args ...any) { panic(fmt.Sprint(args...)) }
