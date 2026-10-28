//go:build unix

package coreslot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/wire"
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
	w, err := c.Enqueue(context.Background(), req)
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

// Shortfall is what a grant runs without, whatever made it: a clamped admission is short by its
// Min minus the whole size, an adoption by what was not free, and a grant that holds every Min, or
// a resource run unbudgeted, is never short.
func TestShortfall(t *testing.T) {
	c := open(t, stateDir(t, "[resources]\ncpu = 4\n"))
	clamped := waitGrant(t, c, Request{Need: map[string]Range{"cpu": {Min: 6, Max: 6}, "gpu": {Min: 1, Max: 1}}})
	if got, want := clamped.Shortfall(), map[string]int{"cpu": 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("clamped admission: shortfall %v, want %v (gpu is unbudgeted, never short)", got, want)
	}
	clamped.Close()

	ctx := context.Background()
	full, err := c.Adopt(ctx, Request{Need: map[string]Range{"cpu": {Min: 2, Max: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	if got := full.Shortfall(); got == nil || len(got) != 0 {
		t.Errorf("adoption of 3 with Min 2: shortfall %#v, want empty", got)
	}
	exact, err := c.Adopt(ctx, Request{Need: map[string]Range{"cpu": {Min: 1, Max: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := exact.Shortfall(); exact.Granted()["cpu"] != 1 || got == nil || len(got) != 0 {
		t.Errorf("adoption of exactly its Min: granted %v, shortfall %#v; want 1 and empty, no zero entry", exact.Granted(), got)
	}
	exact.Close()
	short, err := c.Adopt(ctx, Request{Need: map[string]Range{"cpu": {Min: 2, Max: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	defer short.Close()
	if got, want := short.Shortfall(), map[string]int{"cpu": 1}; short.Granted()["cpu"] != 1 || !reflect.DeepEqual(got, want) {
		t.Errorf("adoption with one token free: granted %v, shortfall %v; want 1 and %v", short.Granted(), got, want)
	}
}

// The child of TestEnvMatchesFiles: it checks, from inside, that the descriptors the
// environment names are the grant's files.
const childEnv = "CORESLOT_ROOT_TEST_CHILD"

func TestMain(m *testing.M) {
	if mode := os.Getenv(nestedEnv); mode != "" {
		os.Exit(passThroughHelper(mode))
	}
	if dir := os.Getenv(childEnv); dir != "" {
		if err := checkChild(dir); err != nil {
			fmt.Fprintln(os.Stderr, "child:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	// A suite run under `coreslot run` inherits the run's grant at fd 3 on, not close-on-exec.
	// Marking it so keeps it in this process: the helpers the tests start, which receive their
	// descriptors through ExtraFiles, never hold the outer tokens past the run.
	if err := closeInheritedOnExec(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot mark the inherited descriptors close-on-exec:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// closeInheritedOnExec marks every open descriptor from 3 on close-on-exec, as listed in the
// directory the system provides for this process's descriptors; without one it does nothing.
func closeInheritedOnExec() error {
	dir := "/dev/fd"
	if runtime.GOOS == "linux" {
		dir = "/proc/self/fd"
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, ent := range ents {
		if fd, err := strconv.Atoi(ent.Name()); err == nil && fd >= 3 {
			unix.CloseOnExec(fd)
		}
	}
	return nil
}

// outerEnv makes TestInheritedDescriptorsStayInTheTestBinary's inner run start its helper.
const outerEnv = "CORESLOT_ROOT_TEST_OUTER"

// A suite run under `coreslot run` inherits the run's hold and tokens at fd 3 on without
// close-on-exec, and every helper a test starts would inherit them too, outliving the run with
// its tokens locked (PROTOCOL.md section 17). The test simulates that outer grant with a file at
// fd 3 of a re-executed test binary, whatever it is itself run under, and requires the helper that
// binary starts to see none of it.
func TestInheritedDescriptorsStayInTheTestBinary(t *testing.T) {
	if os.Getenv(outerEnv) != "" {
		// The inner run: report the outer descriptor it holds, then what its helper holds.
		var st unix.Stat_t
		if err := unix.Fstat(3, &st); err != nil {
			t.Fatalf("fd 3: %v", err)
		}
		fmt.Println("inner", uint64(st.Dev), uint64(st.Ino))
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), nestedEnv+"=fds")
		out, err := cmd.CombinedOutput()
		fmt.Printf("%s", out)
		if err != nil {
			t.Fatalf("helper: %v", err)
		}
		return
	}
	outer, err := os.Create(filepath.Join(t.TempDir(), "outer"))
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(outer.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestInheritedDescriptorsStayInTheTestBinary$")
	cmd.ExtraFiles = []*os.File{outer}
	cmd.Env = append(withoutCoreslot(), outerEnv+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("inner run:\n%s", out)
	if err != nil {
		t.Fatalf("inner run: %v", err)
	}
	mine := fmt.Sprintf("%d %d", uint64(st.Dev), uint64(st.Ino))
	if !strings.Contains(string(out), "inner "+mine+"\n") {
		t.Fatalf("the inner run does not hold the simulated outer descriptor (%s)", mine)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "fd ") && strings.HasSuffix(line, " "+mine) {
			t.Errorf("the helper inherited the outer descriptor: %q", line)
		}
	}
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
		// The hold's descriptor is read-only: the command cannot write into the hold's body.
		fl, err := unix.FcntlInt(3, unix.F_GETFL, 0)
		if err != nil {
			return fmt.Errorf("fd 3: F_GETFL: %v", err)
		}
		if fl&unix.O_ACCMODE != unix.O_RDONLY {
			return fmt.Errorf("fd 3 is open with access mode %#o, want O_RDONLY", fl&unix.O_ACCMODE)
		}
		if _, err := unix.Write(3, []byte("x")); err == nil {
			return fmt.Errorf("fd 3: a write succeeded")
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
	cmd.Env = append(g.Environ(os.Environ()),
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

// Environ is base without its grant context, followed by Env: an inherited CORESLOT_GRANTED_*,
// CORESLOT_HELD_FD, CORESLOT_TOKEN_FDS or CORESLOT_UNBUDGETED never reaches the command, while
// CORESLOT_STATE_DIR and everything else does, in order.
func TestEnvironReplacesGrantContext(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	g := waitGrant(t, open(t, dir), Request{Need: map[string]Range{"cpu": {Min: 2, Max: 2}}})
	base := []string{"PATH=/bin", "CORESLOT_GRANTED_CPU=9", "CORESLOT_GRANTED_GPU=1", "CORESLOT_HELD_FD=7",
		"CORESLOT_TOKEN_FDS=8-9", "CORESLOT_UNBUDGETED=state", "CORESLOT_STATE_DIR=" + dir, "LANG=C"}
	orig := slices.Clone(base)
	got := g.Environ(base)
	want := append([]string{"PATH=/bin", "CORESLOT_STATE_DIR=" + dir, "LANG=C"}, g.Env()...)
	if !slices.Equal(got, want) {
		t.Errorf("Environ =\n%q\nwant\n%q", got, want)
	}
	if !slices.Equal(base, orig) {
		t.Errorf("Environ changed its argument: %q", base)
	}
}

// Every time in a Report is the Unix nanosecond value `status --json` prints, read as a
// time.Time, and 0 is the zero Time; every other field is the engine's snapshot unchanged. The
// state has a value in every field: a clamped hold with an undeclared resource, a fail-open hold,
// a dead hold, a ticket with a pending cancel request, a dead ticket, an ignored entry, locked
// tokens of a resource the configuration no longer declares, the section held, and a degraded
// record, then a malformed one.
func TestReportMatchesTheEngineSnapshot(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\npg = 1\n")
	c := open(t, dir)
	ctx := context.Background()
	waitGrant(t, c, Request{Label: "hold", Need: map[string]Range{"cpu": {Min: 5, Max: 5}, "gpu": {Min: 1, Max: 1},
		"pg": {Min: 1, Max: 1}}})
	w, err := c.Enqueue(ctx, Request{Label: "queued", Priority: 5, Need: map[string]Range{"cpu": {Min: 1, Max: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Cancel() })
	withdrawn, err := c.Enqueue(ctx, Request{Label: "withdrawn"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range r.Tickets {
		if tk.Label != "queued" {
			continue
		}
		id, ok := wire.ParseID(tk.ID)
		if !ok {
			t.Fatalf("ticket id %q", tk.ID)
		}
		if err := c.e.CancelContext(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	fo, err := c.e.FailOpen(spec.Request{Label: "failed open", Need: map[string]spec.Range{"cpu": {Min: 1, Max: 1}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fo.Close() })
	dead, err := c.Adopt(ctx, Request{Label: "dead", Need: map[string]Range{"cpu": {Min: 1, Max: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "degraded"), []byte(`{"reason":"r","first_ns":1700000000123456789,"count":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queue", "not-a-ticket"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// With the section held, a withdrawal and a Close cannot remove their entries: both stay, dead.
	holdSection(t, dir, 4242)
	if err := withdrawn.Cancel(); err != nil {
		t.Fatal(err)
	}
	dead.Close()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[resources]\ncpu = 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ns := func(tm time.Time) int64 {
		if tm.IsZero() {
			return 0
		}
		return tm.UnixNano()
	}
	check := func() Report {
		t.Helper()
		got, err := c.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		want, err := c.e.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		// The library's Open has created the directory, so the engine always reports it.
		back := engine.Report{StateDir: got.StateDir, StateDirExists: true, Format: got.Format,
			FormatPresent: got.FormatPresent, FormatUnderstood: got.FormatUnderstood, Gen: got.Gen,
			ReconciledNS: ns(got.Reconciled), Resources: map[string]engine.ResourceInfo{}, UndeclaredLocked: got.UndeclaredLocked,
			DeadHolds: got.DeadHolds, Cancels: got.CancelRequests, Ignored: got.Ignored,
			Section: engine.SectionInfo{Held: got.Section.Held, PID: got.Section.PID, EnteredNS: ns(got.Section.Entered)}}
		for name, ri := range got.Resources {
			back.Resources[name] = engine.ResourceInfo(ri)
		}
		for _, tk := range got.Tickets {
			need := map[string]spec.Range{}
			for name, rg := range tk.Need {
				need[name] = spec.Range(rg)
			}
			back.Tickets = append(back.Tickets, engine.TicketInfo{Name: tk.Name, ID: tk.ID, Label: tk.Label, Priority: tk.Priority,
				PID: tk.PID, Need: need, CreatedNS: ns(tk.Created), MtimeNS: ns(tk.Refreshed), Dead: tk.Dead})
		}
		for _, h := range got.Holds {
			back.Holds = append(back.Holds, engine.HolderInfo{Name: h.Name, Label: h.Label, PID: h.PID, StartNS: ns(h.Start),
				Grant: h.Granted, Unbudgeted: h.FailOpen, UnbudgetedResources: h.Unbudgeted, Clamped: h.Clamped, Adopted: h.Adopted})
		}
		if d := got.Degraded; d != nil {
			back.Degraded = &engine.DegradedInfo{Reason: d.Reason, FirstNS: ns(d.First), Count: d.Count, Malformed: d.Malformed}
		}
		if !reflect.DeepEqual(back, want) {
			t.Errorf("the Report read back:\n%+v\nthe engine's snapshot:\n%+v", back, want)
		}
		return got
	}

	got := check()
	var failOpen, clamped, unbudgeted, deadTicket bool
	for _, h := range got.Holds {
		failOpen = failOpen || h.FailOpen
		clamped = clamped || h.Clamped["cpu"] == 5
		unbudgeted = unbudgeted || slices.Equal(h.Unbudgeted, []string{"gpu"})
	}
	for _, tk := range got.Tickets {
		deadTicket = deadTicket || (tk.Label == "withdrawn" && tk.Dead)
	}
	if !failOpen || !clamped || !unbudgeted || !deadTicket || len(got.DeadHolds) != 1 || len(got.CancelRequests) != 1 ||
		got.UndeclaredLocked["pg"] != 1 || got.Ignored["queue"] == 0 || !got.Section.Held {
		t.Errorf("a field the scenario sets is empty: fail-open %v, clamped %v, unbudgeted %v, dead ticket %v, "+
			"dead holds %v, cancels %v, undeclared %v, ignored %v, section %+v",
			failOpen, clamped, unbudgeted, deadTicket, got.DeadHolds, got.CancelRequests, got.UndeclaredLocked, got.Ignored, got.Section)
	}
	if got.Degraded == nil || got.Degraded.First.UnixNano() != 1700000000123456789 || got.Section.Entered.UnixNano() != 1 {
		t.Errorf("degraded %+v, section entered %v: want the recorded nanoseconds", got.Degraded, got.Section.Entered)
	}

	if err := os.WriteFile(filepath.Join(dir, "degraded"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := check(); got.Degraded == nil || !got.Degraded.Malformed {
		t.Errorf("degraded %+v after a malformed record, want Malformed", got.Degraded)
	}
}

// A Report's FormatError and ConfigError are *StateErrors, nil when FORMAT and the configuration
// are usable; the engine's texts are what `status --json` prints.
func TestReportErrorsAreStateErrors(t *testing.T) {
	dir := stateDir(t, "")
	c := open(t, dir)
	if r, err := c.Snapshot(); err != nil || r.FormatError != nil || r.ConfigError != nil {
		t.Fatalf("a usable directory: format error %v, config error %v, %v", r.FormatError, r.ConfigError, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "FORMAT"), []byte("not a format\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[resources]\ncpu = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	want, err := c.e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var fe, ce *StateError
	if !errors.As(got.FormatError, &fe) || "coreslot state error: "+fe.Reason != want.FormatError {
		t.Errorf("FormatError %v (%T), want a *StateError whose reason is %q", got.FormatError, got.FormatError, want.FormatError)
	}
	if !errors.As(got.ConfigError, &ce) || ce.Reason != want.ConfigError {
		t.Errorf("ConfigError %v (%T), want a *StateError whose reason is %q", got.ConfigError, got.ConfigError, want.ConfigError)
	}
}

// The Waiter's concurrency contract: the first Poll or Wait to admit wins and the others return the
// same grant; Cancel after admission is ErrAlreadyAdmitted; Cancel before it makes Wait return
// ErrCanceled.
func TestConcurrentPollWaitCancel(t *testing.T) {
	dir := stateDir(t, "")
	c := open(t, dir)
	sawAdmitted, sawCancelled := false, false
	for i := 0; i < 30; i++ {
		w, err := c.Enqueue(context.Background(), Request{Need: map[string]Range{"cpu": {Min: 1, Max: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		// record keeps a grant and an error returned together as both, so the checks of either
		// outcome below fail on such a pair.
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
					record(w.Poll())
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
				if !errors.Is(err, ErrCanceled) {
					t.Fatalf("iteration %d: after Cancel: %v, want ErrCanceled", i, err)
				}
			}
			if len(errs) == 0 {
				t.Fatalf("iteration %d: Wait returned neither a grant nor ErrCanceled", i)
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
	if n, err := c.Available("cpu"); err != nil || n != 3 {
		t.Fatalf("Available = %d, %v; want the state directory's configuration (3)", n, err)
	}
	cfg := filepath.Join(dir, "config.toml")
	if err := os.Chmod(cfg, 0o620); err != nil {
		t.Fatal(err)
	}
	var se *StateError
	if _, err := c.Available("cpu"); !errors.As(err, &se) {
		t.Errorf("group-writable config.toml: %v, want a *StateError", err)
	}
	if _, err := Open(Options{StateDir: dir}); !errors.As(err, &se) {
		t.Errorf("Open with a group-writable config.toml: %v, want a *StateError", err)
	}
}

// A Client holds nothing to release today: Close returns nil, every time.
func TestClientClose(t *testing.T) {
	c := open(t, stateDir(t, ""))
	for i := range 2 {
		if err := c.Close(); err != nil {
			t.Errorf("Close %d: %v, want nil", i, err)
		}
	}
}

func TestUsageErrorBeforeQueueing(t *testing.T) {
	c := open(t, stateDir(t, ""))
	var ue *UsageError
	if _, err := c.Enqueue(context.Background(), Request{Need: map[string]Range{"cpu": {Min: 3, Max: 2}}}); !errors.As(err, &ue) {
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
	w, err := c.Enqueue(context.Background(), Request{Need: map[string]Range{"cpu": {Min: 1, Max: 1}}})
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
