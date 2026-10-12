//go:build unix

package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// ---------------------------------------------------------------- clock and fixtures

// fakeClock is an injected clock: both readings move only when the test advances them, so
// staleness never depends on scheduling or on another process's clock.
type fakeClock struct {
	mu   sync.Mutex
	mono time.Duration
	real int64
}

// newFakeClock starts the wall clock an hour ahead, so that a helper process enqueueing on its
// real clock after the test's clock exists still sorts ahead of the test's tickets.
func newFakeClock() *fakeClock {
	return &fakeClock{mono: time.Hour, real: time.Now().Add(time.Hour).UnixNano()}
}

func (c *fakeClock) clock() Clock {
	return Clock{
		Realtime: func() int64 {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.real
		},
		Monotonic: func() time.Duration {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.mono
		},
	}
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mono += d
	c.real += int64(d)
}

const testCPUs = 8

// newStateDir creates a state directory whose config.toml holds cfg (none when cfg is empty).
func newStateDir(t *testing.T, cfg string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(dir, store.ConfigFile), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newEngine(t *testing.T, dir string, clk *fakeClock) *Engine {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := SystemClock()
	if clk != nil {
		c = clk.clock()
	}
	return New(st, &config.Watcher{Path: filepath.Join(dir, store.ConfigFile)},
		func() (int, error) { return testCPUs, nil }, c)
}

func need(spec string) spec.Request {
	return needLabel(spec, "")
}

func needLabel(arg, label string) spec.Request {
	var args []string
	if arg != "" {
		args = strings.Split(arg, ",")
	}
	n, err := spec.ParseNeeds(args)
	if err != nil {
		panic(err)
	}
	return spec.Request{Label: label, Need: n}
}

func enqueue(t *testing.T, e *Engine, req spec.Request) *Ticket {
	t.Helper()
	tk, err := e.EnqueueContext(context.Background(), req)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	t.Cleanup(func() { _ = tk.Withdraw() })
	return tk
}

// step runs one Step and fails the test on an error.
func step(t *testing.T, tk *Ticket) StepResult {
	t.Helper()
	r, err := tk.Step(context.Background())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if a, ok := r.(Admitted); ok {
		t.Cleanup(func() { a.Close() })
	}
	return r
}

func position(t *testing.T, r StepResult) int {
	t.Helper()
	w, ok := r.(Waiting)
	if !ok {
		t.Fatalf("want Waiting, got %#v", r)
	}
	return w.Position
}

func isAdmitted(r StepResult) bool {
	_, ok := r.(Admitted)
	return ok
}

// lockedCount counts the locked tokens of res in a section of its own.
func lockedCount(t *testing.T, e *Engine, res string) int {
	t.Helper()
	sec, err := e.st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	n, err := sec.CountLocked(res)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// ---------------------------------------------------------------- helper processes

// A helper process re-executes the test binary into TestHelperProcess, does what its mode
// says, prints "ready" (and a detail), then executes one command per stdin line, answering
// "ok", until stdin reaches EOF or it is killed.
const (
	helperModeEnv = "CORESLOT_ENGINE_HELPER"
	helperDirEnv  = "CORESLOT_ENGINE_HELPER_DIR"
	helperArgEnv  = "CORESLOT_ENGINE_HELPER_ARG"
)

func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	dir, arg := os.Getenv(helperDirEnv), os.Getenv(helperArgEnv)
	fail := func(err error) {
		fmt.Println("error:", err)
		os.Exit(2)
	}
	st, err := store.Open(dir)
	if err != nil {
		fail(err)
	}
	e := New(st, &config.Watcher{Path: filepath.Join(dir, store.ConfigFile)},
		func() (int, error) { return testCPUs, nil }, SystemClock())
	var keep []any
	detail := ""
	var tk *Ticket
	switch mode {
	case "enqueue": // publish a ticket and never step it: it is refreshed only on command
		tk, err = e.EnqueueContext(context.Background(), need(arg))
		if err != nil {
			fail(err)
		}
		detail = tk.Name()
	case "section", "section-marked": // hold the admission section; marked: admit.lock names this pid
		sec, err := st.Enter(context.Background())
		if err != nil {
			fail(err)
		}
		if mode == "section-marked" {
			if _, _, err := sec.Format(formats()); err != nil {
				fail(err)
			}
		}
		keep = append(keep, sec)
	case "lock": // lock each token path listed, one per line
		for _, p := range strings.Split(arg, "\n") {
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				fail(err)
			}
			f, err := store.OpenFile(p, os.O_RDONLY|os.O_CREATE)
			if err != nil {
				fail(err)
			}
			if err := lockNB(f); err != nil {
				fail(err)
			}
			keep = append(keep, f)
		}
	default:
		fail(fmt.Errorf("unknown mode %q", mode))
	}
	fmt.Println("ready", detail)
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		switch in.Text() {
		case "refresh":
			if err := tk.refresh(tk.e.clock.Monotonic()); err != nil {
				fail(err)
			}
		default:
			fail(fmt.Errorf("unknown command %q", in.Text()))
		}
		fmt.Println("ok")
	}
	_ = keep
	os.Exit(0)
}

type helper struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	detail string
	once   sync.Once
}

func startHelper(t *testing.T, mode, dir, arg string) *helper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), helperModeEnv+"="+mode, helperDirEnv+"="+dir, helperArgEnv+"="+arg)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &helper{cmd: cmd, stdin: stdin, lines: make(chan string, 16)}
	t.Cleanup(h.stop)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			h.lines <- sc.Text()
		}
		close(h.lines)
	}()
	l := h.read(t)
	detail, ok := strings.CutPrefix(l, "ready")
	if !ok {
		t.Fatalf("helper %s: %q", mode, l)
	}
	h.detail = strings.TrimSpace(detail)
	return h
}

func (h *helper) read(t *testing.T) string {
	t.Helper()
	select {
	case l, ok := <-h.lines:
		if !ok {
			t.Fatal("helper exited")
		}
		return l
	case <-time.After(60 * time.Second):
		t.Fatal("helper did not answer within 60 s")
	}
	return ""
}

func (h *helper) command(t *testing.T, c string) {
	t.Helper()
	if _, err := fmt.Fprintln(h.stdin, c); err != nil {
		t.Fatal(err)
	}
	if l := h.read(t); l != "ok" {
		t.Fatalf("helper %s: %q", c, l)
	}
}

func (h *helper) stop() {
	h.once.Do(func() {
		h.stdin.Close()
		_ = h.cmd.Wait()
	})
}

func (h *helper) kill(t *testing.T) {
	t.Helper()
	h.once.Do(func() {
		_ = h.cmd.Process.Kill()
		_ = h.cmd.Wait()
		h.stdin.Close()
	})
}

// lockNB takes flock(LOCK_EX|LOCK_NB) on f, failing when another description holds it.
func lockNB(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

// tokenPaths lists res/<res>/<i> for i in [from, to].
func tokenPaths(dir, res string, from, to int) string {
	var ps []string
	for i := from; i <= to; i++ {
		ps = append(ps, filepath.Join(dir, store.ResDir, res, strconv.Itoa(i)))
	}
	return strings.Join(ps, "\n")
}

// ---------------------------------------------------------------- ordering, cancel, reconcile

func TestFIFOAtEqualPriority(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 1\n")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	a := enqueue(t, e, needLabel("cpu=1", "a"))
	clk.advance(time.Millisecond)
	b := enqueue(t, e, needLabel("cpu=1", "b"))
	clk.advance(time.Millisecond)
	c := enqueue(t, e, needLabel("cpu=1", "c"))

	var order []string
	grants := map[string]*Grant{}
	waiting := map[string]*Ticket{"a": a, "b": b, "c": c}
	for round := 0; round < 12 && len(order) < 3; round++ {
		clk.advance(time.Second)
		// Step the later tickets first: order must come from the queue, not from call order.
		for _, name := range []string{"c", "b", "a"} {
			tk, ok := waiting[name]
			if !ok {
				continue
			}
			if r := step(t, tk); isAdmitted(r) {
				order = append(order, name)
				grants[name] = r.(Admitted).Grant
				delete(waiting, name)
			} else if name == "c" && len(order) == 0 && position(t, r) != 3 {
				t.Errorf("c: position %d with a and b ahead, want 3", position(t, r))
			}
		}
		// The holder finishes two rounds after its admission.
		if round%2 == 1 {
			for _, g := range grants {
				g.Close()
			}
		}
	}
	if got := strings.Join(order, ","); got != "a,b,c" {
		t.Fatalf("admission order %q, want a,b,c", got)
	}
}

func TestPriorityAgingViaKey(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 1\n")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	blocker := enqueue(t, e, needLabel("cpu=1", "blocker"))
	r := step(t, blocker)
	if !isAdmitted(r) {
		t.Fatal("blocker not admitted")
	}
	holder := r.(Admitted).Grant

	a := enqueue(t, e, needLabel("cpu=1", "a")) // priority 0 at r0
	clk.advance(30 * time.Second)
	reqB := needLabel("cpu=1", "b")
	reqB.Priority = 1 // arrives 30 s later with one point: key r0-30 s, ahead of a
	b := enqueue(t, e, reqB)
	clk.advance(31 * time.Second)
	reqC := needLabel("cpu=1", "c")
	reqC.Priority = 1 // arrives 61 s after a with one point: key r0+1 s, behind a
	c := enqueue(t, e, reqC)
	if !(b.key.Less(a.key) && a.key.Less(c.key)) {
		t.Fatalf("keys: a %v b %v c %v, want b < a < c", a.key, b.key, c.key)
	}
	holder.Close()

	var order []string
	waiting := map[string]*Ticket{"a": a, "b": b, "c": c}
	var last *Grant
	for round := 0; round < 12 && len(order) < 3; round++ {
		clk.advance(time.Second)
		for _, name := range []string{"c", "a", "b"} {
			if tk, ok := waiting[name]; ok {
				if r := step(t, tk); isAdmitted(r) {
					order = append(order, name)
					last = r.(Admitted).Grant
					delete(waiting, name)
				}
			}
		}
		if round%2 == 1 && last != nil {
			last.Close()
		}
	}
	if got := strings.Join(order, ","); got != "b,a,c" {
		t.Fatalf("admission order %q, want b,a,c", got)
	}
}

func TestStoppedHeadIsPassedOver(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	h := startHelper(t, "enqueue", dir, "cpu=1") // enqueues and never refreshes
	s := enqueue(t, e, need("cpu=1"))

	if p := position(t, step(t, s)); p != 2 {
		t.Fatalf("position %d behind a fresh ticket, want 2", p)
	}
	for sec := 1; sec <= 9; sec++ { // one election per second: an absence would not be watched
		clk.advance(time.Second)
		if p := position(t, step(t, s)); p != 2 || s.attempts != 0 {
			t.Fatalf("%d s without a change: position %d, %d attempts; want 2 and none", sec, p, s.attempts)
		}
	}
	clk.advance(time.Second) // the next election, once per second
	if r := step(t, s); !isAdmitted(r) {
		t.Fatalf("10 s without a change: want the successor admitted, got %#v", r)
	}
	if dead, err := e.st.Probe(store.QueueDir, h.detail); err != nil || dead {
		t.Fatalf("the stopped head's ticket: dead %v, %v; want live (only passed over)", dead, err)
	}
}

// A waiter that was itself away for 15 s (stopped, suspended) does not count that time as
// watched (PROTOCOL.md section 10.2): the stopped head is passed over only after 10 s of
// elections once the waiter is back, not at its first election.
func TestWaiterAbsenceIsNotWatched(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "enqueue", dir, "cpu=1") // enqueues and never refreshes
	s := enqueue(t, e, need("cpu=1"))

	if p := position(t, step(t, s)); p != 2 {
		t.Fatalf("position %d behind a fresh ticket, want 2", p)
	}
	clk.advance(15 * time.Second) // the waiter itself made no election for 15 s
	if r := step(t, s); isAdmitted(r) || position(t, r) != 2 || s.attempts != 0 {
		t.Fatalf("first election after the waiter's 15 s absence: %#v, %d attempts; want position 2 (the absence is not watched)", r, s.attempts)
	}
	for sec := 1; sec <= 9; sec++ {
		clk.advance(time.Second)
		if p := position(t, step(t, s)); p != 2 || s.attempts != 0 {
			t.Fatalf("%d s after the return: position %d, %d attempts; want 2 and none", sec, p, s.attempts)
		}
	}
	clk.advance(time.Second) // 10 s of elections after the return
	if r := step(t, s); !isAdmitted(r) {
		t.Fatalf("10 s of watching after the return: want admitted, got %#v", r)
	}
}

func TestStalledTicketResumesItsPlace(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 0)) // the successor cannot be admitted
	h := startHelper(t, "enqueue", dir, "cpu=1")
	s := enqueue(t, e, need("cpu=8"))

	if p := position(t, step(t, s)); p != 2 {
		t.Fatalf("position %d, want 2", p)
	}
	for sec := 1; sec <= 9; sec++ { // one election per second: an absence would not be watched
		clk.advance(time.Second)
		if p := position(t, step(t, s)); p != 2 {
			t.Fatalf("%d s without a change: position %d, want 2", sec, p)
		}
	}
	clk.advance(time.Second)
	if p := position(t, step(t, s)); p != 1 || s.attempts != 1 {
		t.Fatalf("stalled ticket ahead: position %d, %d attempts; want head with one attempt", p, s.attempts)
	}
	h.command(t, "refresh")
	clk.advance(time.Second)
	if p := position(t, step(t, s)); p != 2 || s.attempts != 1 {
		t.Fatalf("after the refresh: position %d, %d attempts; want 2 and no new attempt", p, s.attempts)
	}
	for sec := 1; sec <= 9; sec++ {
		clk.advance(time.Second)
		if p := position(t, step(t, s)); p != 2 {
			t.Fatalf("%d s after the observed refresh: position %d, want 2", sec, p)
		}
	}
	clk.advance(time.Second)
	if p := position(t, step(t, s)); p != 1 {
		t.Fatalf("10 s after the observed refresh: position %d, want 1", p)
	}
}

func TestDeadTicketPassedOverImmediately(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	h := startHelper(t, "enqueue", dir, "cpu=1")
	s := enqueue(t, e, need("cpu=1"))
	if p := position(t, step(t, s)); p != 2 {
		t.Fatalf("position %d, want 2", p)
	}
	h.kill(t)
	clk.advance(time.Second) // the next election; far below the 10 s of a stall
	if r := step(t, s); !isAdmitted(r) {
		t.Fatalf("dead ticket ahead: want admitted at once, got %#v", r)
	}
}

// blocked returns a ticket that cannot be admitted because a helper holds the only cpu token.
func blocked(t *testing.T, cfg string) (dir string, e *Engine, clk *fakeClock, tk *Ticket) {
	t.Helper()
	dir = newStateDir(t, cfg)
	clk = newFakeClock()
	e = newEngine(t, dir, clk)
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 0))
	tk = enqueue(t, e, need("cpu=1"))
	if r := step(t, tk); isAdmitted(r) {
		t.Fatal("admitted while the only token is held")
	}
	return dir, e, clk, tk
}

func TestCancelThroughFile(t *testing.T) {
	dir, _, clk, tk := blocked(t, "[resources]\ncpu = 1\n")
	other := newEngine(t, dir, nil) // another client
	if err := other.CancelContext(context.Background(), tk.ID()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	cancelPath := filepath.Join(dir, store.CancelDir, tk.ID().String())
	if !exists(cancelPath) {
		t.Fatal("no cancel request published")
	}
	clk.advance(10 * time.Millisecond) // not even an election is due: the check is on every poll
	if _, err := tk.Step(context.Background()); !errors.Is(err, ErrCancelled) {
		t.Fatalf("Step after the cancel: %v, want ErrCancelled", err)
	}
	if exists(filepath.Join(dir, store.QueueDir, tk.Name())) || exists(cancelPath) {
		t.Error("the owner did not remove its ticket and the cancel request")
	}
	if _, err := tk.Step(context.Background()); !errors.Is(err, ErrCancelled) {
		t.Errorf("a second Step: %v, want ErrCancelled", err)
	}
	if err := other.CancelContext(context.Background(), tk.ID()); !errors.Is(err, ErrNoTicket) {
		t.Errorf("cancelling a gone ticket: %v, want ErrNoTicket", err)
	}
}

func TestLateCancelHasNoEffect(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	tk := enqueue(t, e, need("cpu=2"))
	id := tk.ID()
	r := step(t, tk)
	if !isAdmitted(r) {
		t.Fatal("not admitted")
	}
	other := newEngine(t, dir, nil)
	if err := other.CancelContext(context.Background(), id); !errors.Is(err, ErrNoTicket) {
		t.Fatalf("Cancel after admission: %v, want ErrNoTicket", err)
	}
	if exists(filepath.Join(dir, store.CancelDir, id.String())) {
		t.Error("a cancel request was published for an admitted ticket")
	}
	if n := lockedCount(t, other, "cpu"); n != 2 {
		t.Errorf("locked %d after the late cancel, want the grant's 2", n)
	}
	r2, err := tk.Step(context.Background())
	if err != nil || r2.(Admitted).Grant != r.(Admitted).Grant {
		t.Errorf("Step after admission: %v %v, want the same grant", r2, err)
	}
	if err := tk.Withdraw(); !errors.Is(err, ErrAlreadyAdmitted) {
		t.Errorf("Withdraw after admission: %v, want ErrAlreadyAdmitted", err)
	}
	if n := lockedCount(t, other, "cpu"); n != 2 {
		t.Errorf("locked %d after Withdraw, want 2", n)
	}
}

func TestWithdrawNeedsNoSection(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	tk := enqueue(t, e, need("cpu=1"))
	startHelper(t, "section", dir, "")

	done := make(chan error, 1)
	go func() { done <- tk.Withdraw() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Withdraw: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Withdraw waited for the section another process holds")
	}
	dead, err := e.st.Probe(store.QueueDir, tk.Name())
	if err != nil || !dead {
		t.Fatalf("the withdrawn ticket: dead %v, %v; want dead to a third observer", dead, err)
	}
	if _, err := tk.Step(context.Background()); !errors.Is(err, ErrCancelled) {
		t.Errorf("Step after Withdraw: %v, want ErrCancelled", err)
	}
}

func TestReconciliationAtMostEvery30s(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	start := clk.clock().Realtime()

	first := enqueue(t, e, need("cpu=1")) // gen is absent: this section reconciles
	g, err := e.st.ReadGen()
	if err != nil || g.ReconciledNS != start {
		t.Fatalf("first section: reconciled_ns %d (%v), want %d", g.ReconciledNS, err, start)
	}
	first.Withdraw()

	// A dead ticket that only reconciliation removes.
	deadID := wire.ID{OrderKey: 1, PID: 999999, Seq: 1}
	body, _ := wire.Encode(wire.Ticket{Need: map[string]spec.Range{"cpu": {Min: 1, Max: 1}}})
	deadPath := filepath.Join(dir, store.QueueDir, wire.TicketName(deadID, ""))
	if err := os.WriteFile(deadPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	sections := 2 // the enqueue and the withdrawal above
	for sections < 100 {
		clk.advance(250 * time.Millisecond) // 100 sections in under 25 s
		tk := enqueue(t, e, need("cpu=1"))
		if err := tk.Withdraw(); err != nil {
			t.Fatal(err)
		}
		sections += 2
		g, err := e.st.ReadGen()
		if err != nil || g.ReconciledNS != start {
			t.Fatalf("after %d sections within 30 s: reconciled_ns %d, want %d (a second reconciliation)",
				sections, g.ReconciledNS, start)
		}
	}
	if !exists(deadPath) {
		t.Fatal("the dead ticket was removed within 30 s of the last reconciliation")
	}
	clk.advance(30*time.Second - time.Duration(clk.clock().Realtime()-start) + time.Millisecond)
	enqueue(t, e, need("cpu=1"))
	if g, _ := e.st.ReadGen(); g.ReconciledNS == start {
		t.Fatal("no reconciliation more than 30 s after the last one")
	}
	if exists(deadPath) {
		t.Error("reconciliation left the dead ticket")
	}
}

func TestSeqWraps(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock() // the wall clock does not move: every ticket gets the same order key
	e := newEngine(t, dir, clk)
	var mu sync.Mutex
	next := 999998
	e.seq = func() int {
		mu.Lock()
		defer mu.Unlock()
		next = wire.NextSeq(next)
		return next
	}
	var ids []wire.ID
	for i := 0; i < 3; i++ {
		ids = append(ids, enqueue(t, e, need("cpu=1")).ID())
	}
	for i, want := range []int{999999, 0, 1} {
		if ids[i].Seq != want {
			t.Errorf("ticket %d: seq %d, want %d", i, ids[i].Seq, want)
		}
		name := wire.TicketName(ids[i], "")
		if id, ok := wire.ParseName(name); !ok || id != ids[i] || !strings.HasSuffix(name, fmt.Sprintf("-%06d", want)) {
			t.Errorf("ticket %d: name %q does not carry seq %06d", i, name, want)
		}
	}

	// Restarting the counter under the same key collides with every live name: the writer takes
	// its next seq until a name is free, across the wrap.
	mu.Lock()
	next = 999998
	mu.Unlock()
	tk := enqueue(t, e, need("cpu=1"))
	if tk.ID().Seq != 2 {
		t.Errorf("after collisions on 999999, 0 and 1: seq %d, want 2", tk.ID().Seq)
	}
	entries, _, err := e.st.List(store.QueueDir)
	if err != nil || len(entries) != 4 {
		t.Fatalf("queue: %d entries (%v), want 4 distinct tickets", len(entries), err)
	}
}

// ---------------------------------------------------------------- protocol clarifications

func readGen(t *testing.T, e *Engine) int64 {
	t.Helper()
	g, err := e.st.ReadGen()
	if err != nil {
		t.Fatal(err)
	}
	return g.N
}

// Every section that publishes or removes a ticket or hold bumps gen once; a cancel publication
// does not (PROTOCOL.md section 7.8).
func TestOwnRemovalsBumpGenAndCancelDoesNot(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 1\n")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)

	w := enqueue(t, e, need("cpu=1"))
	before := readGen(t, e)
	if err := w.Withdraw(); err != nil {
		t.Fatal(err)
	}
	if got := readGen(t, e); got != before+1 {
		t.Errorf("withdrawal with removal: gen %d -> %d, want one bump", before, got)
	}

	holder := enqueue(t, e, need("cpu=1"))
	g := step(t, holder).(Admitted).Grant
	c := enqueue(t, e, need("cpu=1"))
	step(t, c)
	before = readGen(t, e)
	if err := newEngine(t, dir, nil).CancelContext(context.Background(), c.ID()); err != nil {
		t.Fatal(err)
	}
	if got := readGen(t, e); got != before {
		t.Errorf("cancel publication: gen %d -> %d, want unchanged", before, got)
	}
	if _, err := c.Step(context.Background()); !errors.Is(err, ErrCancelled) {
		t.Fatalf("Step: %v, want ErrCancelled", err)
	}
	if got := readGen(t, e); got != before+1 {
		t.Errorf("the owner's removal after the cancel: gen %d -> %d, want one bump", before, got)
	}

	before = readGen(t, e)
	g.Close()
	if got := readGen(t, e); got != before+1 {
		t.Errorf("removal of the own hold: gen %d -> %d, want one bump", before, got)
	}
	if entries, _, _ := e.st.List(store.HoldsDir); len(entries) != 0 {
		t.Errorf("the closed grant's hold is still listed: %v", entries)
	}
}

// A cancel request that already exists means the cancellation is already requested: not an
// error, and no second request (PROTOCOL.md section 11).
func TestCancelAlreadyRequestedIsNotAnError(t *testing.T) {
	_, e, _, tk := blocked(t, "[resources]\ncpu = 1\n")
	for i := 0; i < 2; i++ {
		if err := e.CancelContext(context.Background(), tk.ID()); err != nil {
			t.Fatalf("Cancel #%d: %v", i+1, err)
		}
	}
	if entries, _, _ := e.st.List(store.CancelDir); len(entries) != 1 {
		t.Errorf("cancel/: %d entries, want 1", len(entries))
	}
}

// countEntries replaces the section-entry seams with counting wrappers for the rest of the test.
func countEntries(t *testing.T) (enters, tryEnters *int) {
	t.Helper()
	var e, te int
	oldEnter, oldTry := enterSection, tryEnterSection
	enterSection = func(st *store.Store, ctx context.Context) (*store.Section, error) {
		e++
		return oldEnter(st, ctx)
	}
	tryEnterSection = func(st *store.Store) (*store.Section, bool, error) {
		te++
		return oldTry(st)
	}
	t.Cleanup(func() { enterSection, tryEnterSection = oldEnter, oldTry })
	return &e, &te
}

// The resume bump never delays a refresh (PROTOCOL.md section 10.2): it is a single attempt at
// the section, made after the refresh, and a busy section only postpones it to the next refresh.
func TestResumeBumpNeverDelaysRefresh(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 7)) // nobody can be admitted
	ahead := enqueue(t, e, need("cpu=1"))
	tk := enqueue(t, e, need("cpu=1"))
	step(t, ahead)
	step(t, tk)
	section := startHelper(t, "section", dir, "")
	before := readGen(t, e)

	clk.advance(11 * time.Second) // both were suspended: a refresh gap above 10 s
	step(t, ahead)                // refreshes, so tk sees it live and stays behind it
	enters, tryEnters := countEntries(t)
	step(t, tk)
	if *enters != 0 || *tryEnters != 1 {
		t.Errorf("the Step after the gap entered %d times blocking, %d times single-attempt; want 0 and 1",
			*enters, *tryEnters)
	}
	fi, err := os.Stat(filepath.Join(dir, store.QueueDir, tk.Name()))
	if err != nil {
		t.Fatal(err)
	}
	if want := clk.clock().Realtime(); fi.ModTime().UnixNano()/1000 != want/1000 {
		t.Errorf("mtime %d after the Step, want the refresh at %d", fi.ModTime().UnixNano(), want)
	}
	if got := readGen(t, e); got != before {
		t.Fatalf("gen %d -> %d while the section is held", before, got)
	}
	section.stop()
	clk.advance(time.Second)
	step(t, tk)
	if got := readGen(t, e); got != before+1 {
		t.Errorf("after the section is free: gen %d -> %d, want the resume bump", before, got)
	}
}

// Refresh-to-refresh stays within the bound RefreshEvery documents while the section is busy and
// the waiter, as head, keeps trying to enter it (PROTOCOL.md section 10.2). Deterministic: the
// engine's injected clock, and a section entry that waits out its whole context deadline on that
// clock, so the margin does not depend on host load. The bound is the worst case with the
// mid-step refresh: a Step starting just under RefreshEvery/4 after a refresh does not refresh
// before its entry, which then waits sectionWait, and the caller's next Step starts up to
// RefreshEvery/8 after that: RefreshEvery/4 + sectionWait + RefreshEvery/8 = 1.25 s. Without
// the mid-step refresh a Step starting just under RefreshEvery/2 after a refresh leaves up to
// 1.75 s (1.5 s on this test's 250 ms grid).
func TestRefreshCadenceWithBusySection(t *testing.T) {
	const poll = 250 * time.Millisecond // a caller polling faster than Due asks
	// RefreshEvery/4 + sectionWait + RefreshEvery/8, the documented worst case.
	bound := RefreshEvery/4 + sectionWait + RefreshEvery/8
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	tk := enqueue(t, e, need("cpu=1"))

	oldEnter := enterSection
	enterSection = func(st *store.Store, ctx context.Context) (*store.Section, error) {
		// The section is busy for the whole wait: the entry returns at its ctx deadline.
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("a Step's section entry has no deadline")
			return nil, context.DeadlineExceeded
		}
		clk.advance(time.Until(dl))
		return nil, context.DeadlineExceeded
	}
	defaultPoint := point
	var refreshes []time.Duration
	point = func(name string) {
		if name == "ticket.refreshed" {
			refreshes = append(refreshes, clk.clock().Monotonic())
		}
	}
	t.Cleanup(func() { enterSection, point = oldEnter, defaultPoint })

	start := clk.clock().Monotonic()
	for clk.clock().Monotonic()-start < 60*time.Second {
		step(t, tk)
		clk.advance(poll)
	}
	end := clk.clock().Monotonic()
	// Every gap counts, including the one from the start and the one up to the end. The fake
	// clock only moves inside the entry stub and between Steps, so there is no Step work to add;
	// time.Until(dl) is a hair under sectionWait, hence the small slack.
	const slack = 5 * time.Millisecond
	prev := start
	for _, r := range append(refreshes, end) {
		if gap := r - prev; gap > bound+slack {
			t.Errorf("refresh gap %v above %v (refreshes at %v, start %v, end %v)", gap, bound, refreshes, start, end)
		}
		prev = r
	}
	if len(refreshes) < 10 {
		t.Errorf("only %d refreshes in 60 s of waiting", len(refreshes))
	}
}

// A real-clock smoke test of the same cadence against a section really held by another process.
// It asserts only the protocol's own bound, RefreshEvery, so it keeps about 1 s of slack over
// the 1.25 s the engine achieves and cannot see the mid-step refresh go missing; the injected
// clock test above is the structural one.
func TestRefreshCadenceWithBusySectionRealClockSmoke(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, nil)
	tk := enqueue(t, e, need("cpu=1"))
	startHelper(t, "section", dir, "")
	var mu sync.Mutex
	var refreshes []time.Time
	defaultPoint := point
	point = func(name string) {
		if name == "ticket.refreshed" {
			mu.Lock()
			refreshes = append(refreshes, time.Now())
			mu.Unlock()
		}
	}
	defer func() { point = defaultPoint }()
	start := time.Now()
	for time.Since(start) < 4500*time.Millisecond {
		if _, err := tk.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(250 * time.Millisecond) // a caller polling faster than Due asks
	}
	end := time.Now()
	mu.Lock()
	defer mu.Unlock()
	// Every gap counts, including the one from the start and the one up to the end.
	prev := start
	for _, r := range append(refreshes, end) {
		if gap := r.Sub(prev); gap > RefreshEvery {
			t.Errorf("refresh gap %v above %v (start %v, refreshes %v, end %v)", gap, RefreshEvery,
				start.Format(time.StampMilli), stamps(refreshes), end.Format(time.StampMilli))
		}
		prev = r
	}
}

// A waiter whose own ticket's name disappears keeps waiting and is admitted; ENOENT on its
// unlink counts as success (PROTOCOL.md section 10.3).
func TestOwnTicketRemovedKeepsWaiting(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	lock := startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 7))
	tk := enqueue(t, e, need("cpu=1"))
	if err := os.Remove(filepath.Join(dir, store.QueueDir, tk.Name())); err != nil {
		t.Fatal(err)
	}
	if r := step(t, tk); isAdmitted(r) {
		t.Fatal("admitted with every token held")
	}
	lock.stop()
	clk.advance(time.Second)
	if r := step(t, tk); !isAdmitted(r) {
		t.Fatalf("want admitted once tokens are free, got %#v", r)
	}
}

// A hold whose body's start_ns differs from its name's is malformed and ignored (PROTOCOL.md
// section 7.4): it is not reported as a holder.
func TestHoldStartMismatchIsIgnored(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	holds := filepath.Join(dir, store.HoldsDir)
	if err := os.Mkdir(holds, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(nameStart, bodyStart uint64, label string) string {
		body, err := wire.Encode(wire.Hold{Label: label, Grant: map[string][]int{}, StartNS: bodyStart})
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(holds, wire.HoldName(nameStart, 4242, 1, label))
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write(1000, 1000, "good")
	bad := write(2000, 2001, "bad")
	startHelper(t, "lock", dir, good+"\n"+bad) // both live
	tk := enqueue(t, e, need("cpu=1"))
	clk.advance(time.Second)
	// The ticket is head and admitted; a second ticket behind it reports the holders.
	step(t, tk)
	behind := enqueue(t, e, need("cpu=8"))
	step(t, behind)
	holders, err := behind.Holders()
	if err != nil {
		t.Fatal(err)
	}
	w := Waiting{Holders: holders}
	labels := map[string]bool{}
	for _, h := range w.Holders {
		labels[h.Label] = true
	}
	if !labels["good"] || labels["bad"] || len(w.Holders) != 2 {
		t.Errorf("holders %+v, want the well-formed hold and the admitted one, not the mismatched one", w.Holders)
	}
}

// Stray entries (a grammar-matching name that is a symlink, a directory or a group-writable
// file) are ignored and counted, never a state error and never trusted (PROTOCOL.md 4.2).
func TestStrayEntriesAreIgnored(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 7))
	tk := enqueue(t, e, need("cpu=1"))
	for _, d := range []string{store.HoldsDir, store.CancelDir} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A directory under the ticket's own cancel name, a symlink ticket ahead of it, and a
	// group-writable hold.
	if err := os.Mkdir(filepath.Join(dir, store.CancelDir, tk.ID().String()), 0o700); err != nil {
		t.Fatal(err)
	}
	ahead := wire.TicketName(wire.ID{OrderKey: 1, PID: 1, Seq: 1}, "")
	if err := os.Symlink(filepath.Join(dir, store.QueueDir, tk.Name()), filepath.Join(dir, store.QueueDir, ahead)); err != nil {
		t.Fatal(err)
	}
	hold := filepath.Join(dir, store.HoldsDir, wire.HoldName(5, 1, 1, ""))
	if err := os.WriteFile(hold, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hold, 0o620); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second)
	if p := position(t, step(t, tk)); p != 1 {
		t.Errorf("position %d behind a stray symlink, want 1 (head)", p)
	}
	r, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if r.Ignored[store.QueueDir] != 1 || r.Ignored[store.HoldsDir] != 1 || r.Ignored[store.CancelDir] != 1 {
		t.Errorf("ignored %v, want one stray entry in each directory", r.Ignored)
	}
	if len(r.Tickets) != 1 || r.Tickets[0].Name != tk.Name() {
		t.Errorf("tickets %+v, want only the real one", r.Tickets)
	}
}

func stamps(ts []time.Time) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Format(time.StampMilli))
	}
	return out
}
