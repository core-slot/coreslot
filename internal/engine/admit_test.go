//go:build unix

package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// readHold decodes the body of the grant's hold.
func readHold(t *testing.T, e *Engine, g *Grant) wire.Hold {
	t.Helper()
	body, err := e.readBody(store.HoldsDir, g.HoldName())
	if err != nil {
		t.Fatal(err)
	}
	h, err := wire.DecodeHold(body)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// lockedIndices probes res/<res>/0..n-1 from a fresh description and lists the locked ones.
func lockedIndices(t *testing.T, e *Engine, res string, n int) []int {
	t.Helper()
	var out []int
	for i := 0; i < n; i++ {
		dead, err := e.st.Probe(filepath.Join(store.ResDir, res), strconv.Itoa(i))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !dead {
			out = append(out, i)
		}
	}
	return out
}

func admitted(t *testing.T, r StepResult) *Grant {
	t.Helper()
	a, ok := r.(Admitted)
	if !ok {
		t.Fatalf("want Admitted, got %#v", r)
	}
	return a.Grant
}

// I2: a live hold lists exactly the tokens it locks.
func TestAdmitTakesExactlyGrant(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	g := admitted(t, step(t, enqueue(t, e, need("cpu=2..3"))))
	h := readHold(t, e, g)
	if !reflect.DeepEqual(h.Grant, map[string][]int{"cpu": {0, 1, 2}}) {
		t.Errorf("hold grant %v, want cpu [0 1 2]", h.Grant)
	}
	if got := lockedIndices(t, e, "cpu", testCPUs); !reflect.DeepEqual(got, h.Grant["cpu"]) {
		t.Errorf("locked %v, hold lists %v: want exactly the same tokens", got, h.Grant["cpu"])
	}
	if got := g.Grant(); got["cpu"] != 3 {
		t.Errorf("Grant() = %v, want cpu 3", got)
	}
	g.Close()
	if got := lockedIndices(t, e, "cpu", testCPUs); len(got) != 0 {
		t.Errorf("after Close, locked %v", got)
	}
	if entries, _, _ := e.st.List(store.HoldsDir); len(entries) != 0 {
		t.Errorf("after Close, holds %v", entries)
	}
}

func TestAdmitElasticGrant(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 4)) // 5 of 8 held: 3 free

	big := enqueue(t, e, need("cpu=4..6"))
	if r := step(t, big); isAdmitted(r) {
		t.Fatal("4..6 admitted with 3 free")
	}
	big.Withdraw()
	g := admitted(t, step(t, enqueue(t, e, need("cpu=2..6"))))
	if h := readHold(t, e, g); !reflect.DeepEqual(h.Grant["cpu"], []int{5, 6, 7}) {
		t.Errorf("2..6 with 3 free: hold %v, want cpu [5 6 7]", h.Grant)
	}
}

// PROTOCOL.md section 12.2: a request above the size waits for the whole resource, runs alone, and
// records it.
func TestOversizeIsClampedAndRunsAlone(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 10\n")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	other := startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 9, 9))
	tk := enqueue(t, e, need("cpu=16..16"))
	if r := step(t, tk); isAdmitted(r) {
		t.Fatal("clamped request admitted with 9 of 10 free")
	}
	other.stop()
	clk.advance(time.Second)
	g := admitted(t, step(t, tk))
	if g.Grant()["cpu"] != 10 || !reflect.DeepEqual(g.Clamped(), map[string]int{"cpu": 16}) {
		t.Errorf("grant %v clamped %v, want cpu 10 and clamped cpu 16", g.Grant(), g.Clamped())
	}
	if h := readHold(t, e, g); !reflect.DeepEqual(h.Clamped, map[string]int{"cpu": 16}) || len(h.Grant["cpu"]) != 10 {
		t.Errorf("hold %+v, want 10 tokens and clamped cpu 16", h)
	}
	if n := lockedCount(t, e, "cpu"); n != 10 {
		t.Errorf("locked %d, want the whole resource", n)
	}
}

// PROTOCOL.md section 12.1: an undeclared resource is not budgeted, listed in the hold, and is not
// degraded mode.
func TestUndeclaredResourceIsUnbudgeted(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	g := admitted(t, step(t, enqueue(t, e, need("cpu=1,gpu=2"))))
	if !reflect.DeepEqual(g.Unbudgeted(), []string{"gpu"}) || g.Grant()["gpu"] != 2 || g.Grant()["cpu"] != 1 {
		t.Errorf("unbudgeted %v grant %v, want [gpu] and cpu 1 gpu 2", g.Unbudgeted(), g.Grant())
	}
	h := readHold(t, e, g)
	if !reflect.DeepEqual(h.UnbudgetedResources, []string{"gpu"}) || h.Unbudgeted {
		t.Errorf("hold %+v, want unbudgeted_resources [gpu] and not unbudgeted", h)
	}
	if _, ok := h.Grant["gpu"]; ok {
		t.Error("the hold lists gpu tokens")
	}
	if exists(filepath.Join(dir, store.DegradedFile)) || exists(filepath.Join(dir, store.ResDir, "gpu")) {
		t.Error("an undeclared resource wrote a degraded record or token files")
	}
	if !slices.Contains(g.Env(), "CORESLOT_GRANTED_GPU=2") {
		t.Errorf("env %v, want CORESLOT_GRANTED_GPU=2", g.Env())
	}
}

// I4: a waiter behind a blocked head whose minimum is every cpu takes nothing, through Step or
// Poll: passing the head (PROTOCOL.md section 10.3) never takes what the head is short of.
func TestOnlyHeadTakesTokens(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 0))
	head := enqueue(t, e, need("cpu=8")) // cannot be admitted: 7 of 8 free
	behind := enqueue(t, e, need("cpu=1"))
	for round := 0; round < 6; round++ {
		clk.advance(time.Second)
		step(t, head)
		var r StepResult
		var err error
		if round%2 == 0 {
			r, err = behind.Poll()
		} else {
			r, err = behind.Step(context.Background())
		}
		if err != nil {
			t.Fatal(err)
		}
		if isAdmitted(r) {
			admitted(t, r).Close()
			t.Fatalf("round %d: the waiter behind the head was admitted (backfill)", round)
		}
		if n := lockedCount(t, e, "cpu"); n != 1 {
			t.Fatalf("round %d: %d tokens locked, want only the helper's 1", round, n)
		}
	}
	if behind.attempts != 0 {
		t.Errorf("the waiter behind tried admission %d times", behind.attempts)
	}
}

// passConfig declares a database slot beside the cpus: a head waiting for the slot, which a
// helper holds, leaves cpus free that a waiter naming no slot may take behind its minimums.
const passConfig = "[resources]\ncpu = 8\npg-local = 1\n"

// A waiter in the front window that fits behind every minimum ahead is admitted while the live
// head keeps waiting, and its extra pass leaves those minimums free; one outside the front window
// does not even enter the section. The head is admitted as soon as its slot frees.
//
// Mutations: dropping the front-window check outside the section makes W enter the section from
// position 5; Plan in place of state.PlanBehind grants W all 8 cpus, and the head then waits on.
func TestPasserAdmittedInFrontWindow(t *testing.T) {
	dir := newStateDir(t, passConfig)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	slot := startHelper(t, "lock", dir, tokenPaths(dir, "pg-local", 0, 0))
	var heads []*Ticket
	for i := 0; i < FrontWindow; i++ {
		heads = append(heads, enqueue(t, e, need("cpu=1,pg-local=1")))
	}
	w := enqueue(t, e, need("cpu=1..8"))
	second := func() StepResult {
		t.Helper()
		for i, h := range heads {
			if r := step(t, h); isAdmitted(r) {
				t.Fatalf("ticket %d, waiting for the slot, was admitted", i)
			}
		}
		enters, tryEnters := countEntries(t)
		r := step(t, w)
		if !isAdmitted(r) && (*enters != 0 || *tryEnters != 0) && len(heads) >= FrontWindow {
			t.Fatalf("W, outside the front window, entered the section (%d, %d times)", *enters, *tryEnters)
		}
		clk.advance(time.Second)
		return r
	}
	for sec := 0; sec < 3; sec++ {
		if r := second(); isAdmitted(r) {
			t.Fatalf("%d s: W admitted from position %d, outside the front window", sec, FrontWindow+1)
		}
	}
	if err := heads[FrontWindow-1].Withdraw(); err != nil {
		t.Fatal(err)
	}
	heads = heads[:FrontWindow-1]
	g := admitted(t, second())
	if got := g.Grant()["cpu"]; got != 8-len(heads) {
		t.Fatalf("W was granted %d cpus, want %d: every cpu minimum ahead stays free", got, 8-len(heads))
	}
	slot.stop()
	if r := step(t, heads[0]); !isAdmitted(r) {
		t.Fatalf("the head was not admitted once its slot freed: %#v", r)
	}
}

// writeTicket publishes body as a ticket at the front of the queue, without bumping gen, and
// keeps it locked by a helper, as a live client's ticket is.
func writeTicket(t *testing.T, dir, body string) (path string, h *helper) {
	t.Helper()
	path = filepath.Join(dir, store.QueueDir, wire.TicketName(wire.ID{OrderKey: 1, PID: 1, Seq: 1}, ""))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, startHelper(t, "lock", dir, path)
}

// A ticket ahead whose body is malformed makes the waiter not pass, where it would fit behind the
// live head: inside the section when it appeared after the waiter's last rescan, and without
// entering the section once a rescan found it. Removed, it no longer blocks.
//
// Mutations: the in-section check ignoring malformed bodies admits W on the first Step after the
// cpus free; rescan forgetting malformed names makes W enter the section after the gen bump.
func TestMalformedTicketAheadBlocksPassing(t *testing.T) {
	dir := newStateDir(t, passConfig)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "lock", dir, tokenPaths(dir, "pg-local", 0, 0))
	busy := startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 6)) // 1 cpu free: the head's minimum
	head := enqueue(t, e, need("cpu=1,pg-local=1"))
	w := enqueue(t, e, need("cpu=1"))
	second := func() (r StepResult, entries int) {
		t.Helper()
		step(t, head)
		enters, tryEnters := countEntries(t)
		r = step(t, w)
		clk.advance(time.Second)
		return r, *enters + *tryEnters
	}
	if r, _ := second(); isAdmitted(r) {
		t.Fatal("W admitted with no cpu free beyond the head's minimum")
	}
	path, bad := writeTicket(t, dir, "{}")
	busy.stop()
	for sec := 0; sec < 3; sec++ {
		r, entries := second()
		if isAdmitted(r) {
			t.Fatalf("%d s: W passed the head with a malformed ticket ahead (unseen at its rescan)", sec)
		}
		if entries != 1 {
			t.Fatalf("%d s: W entered the section %d times, want 1 (the check inside it decides)", sec, entries)
		}
	}
	enqueue(t, e, need("cpu=1")) // bumps gen: W rescans and finds the malformed name
	for sec := 0; sec < 3; sec++ {
		if r, entries := second(); isAdmitted(r) || entries != 0 {
			t.Fatalf("%d s: behind a malformed ticket W was admitted (%v) or entered the section %d times, want neither",
				sec, isAdmitted(r), entries)
		}
	}
	bad.stop()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	enqueue(t, e, need("cpu=1")) // bumps gen
	if r, _ := second(); !isAdmitted(r) {
		t.Fatalf("the malformed ticket removed: W was not admitted behind the head (%#v)", r)
	}
}

// A STALLED ticket ahead does not count: its minimum is not subtracted. The stopped ticket S needs
// every cpu, so W cannot pass while S is live; once S is STALLED, W passes the live head L.
//
// Mutation: counting STALLED tickets in the minimums ahead leaves W waiting.
func TestStalledTicketAheadIsIgnoredByPasser(t *testing.T) {
	dir := newStateDir(t, passConfig)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "lock", dir, tokenPaths(dir, "pg-local", 0, 0))
	startHelper(t, "enqueue", dir, "cpu=8") // S: the real clock sorts it ahead of L, stopped
	l := enqueue(t, e, need("cpu=1,pg-local=1"))
	w := enqueue(t, e, need("cpu=1"))
	for sec := 0; sec <= 10; sec++ {
		if r := step(t, l); isAdmitted(r) {
			t.Fatalf("%d s: L, waiting for the slot, was admitted", sec)
		}
		r := step(t, w)
		if sec < 10 && isAdmitted(r) {
			t.Fatalf("%d s: W passed the live S, whose minimum is every cpu", sec)
		}
		if sec == 10 && !isAdmitted(r) {
			t.Fatalf("10 s: S is STALLED, yet W did not pass L (%#v)", r)
		}
		clk.advance(time.Second)
	}
}

// PROTOCOL.md section 13.1: a cancel published before the head's admitting section wins.
func TestCancelPublishedBeforeAdmissionWins(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	head := enqueue(t, e, need("cpu=2"))

	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defaultPoint := point
	point = func(name string) {
		if name == "admit.before-enter" {
			once.Do(func() { close(reached) })
			<-release
		}
	}
	type result struct {
		r   StepResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := head.Step(context.Background())
		done <- result{r, err}
	}()
	defer func() { point = defaultPoint }()
	<-reached
	if err := newEngine(t, dir, nil).CancelContext(context.Background(), head.ID()); err != nil { // its own completed section
		t.Fatalf("Cancel: %v", err)
	}
	close(release)
	res := <-done
	if !errors.Is(res.err, ErrCanceled) {
		if a, ok := res.r.(Admitted); ok {
			a.Close()
		}
		t.Fatalf("head Step: %#v, %v; want ErrCanceled", res.r, res.err)
	}
	if n := lockedCount(t, e, "cpu"); n != 0 {
		t.Errorf("%d tokens locked by a cancelled head", n)
	}
	if exists(filepath.Join(dir, store.CancelDir, head.ID().String())) {
		t.Error("the cancel request was not removed")
	}
}

func TestAdoptReportsShortfall(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 10\n")
	e := newEngine(t, dir, newFakeClock())
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 7))
	enqueue(t, e, need("cpu=1")) // a waiter: adoption does not queue behind it
	g, err := e.AdoptContext(context.Background(), need("cpu=3"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	h := readHold(t, e, g)
	if !reflect.DeepEqual(h.Grant, map[string][]int{"cpu": {8, 9}}) || !h.Adopted {
		t.Errorf("hold %+v, want adopted cpu [8 9]", h)
	}
	if short := g.Shortfall(); !reflect.DeepEqual(short, map[string]int{"cpu": 1}) {
		t.Errorf("shortfall %v, want cpu 1", short)
	}
	if n := lockedCount(t, e, "cpu"); n > 10 {
		t.Errorf("I3: %d locked, size 10", n)
	}
}

// I3 for adoption: locks at or above the size count, so a shrink leaves less to adopt.
func TestAdoptCountsLocksAboveSize(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 10\n")
	e := newEngine(t, dir, newFakeClock())
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 4)+"\n"+tokenPaths(dir, "cpu", 10, 13))
	g, err := e.AdoptContext(context.Background(), need("cpu=3..5"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if got, short := g.Grant()["cpu"], g.Shortfall(); got != 1 || !reflect.DeepEqual(short, map[string]int{"cpu": 2}) {
		t.Errorf("grant %d shortfall %v, want 1 and cpu 2", got, short)
	}
	if n := lockedCount(t, e, "cpu"); n != 10 {
		t.Errorf("I3: %d locked at every index, want exactly the size 10", n)
	}
}

// listing is a recursive listing of dir with every attribute a write would change.
func listing(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(&b, "%s %v %d %d\n", rel, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func writeFormat(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, store.FormatFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// PROTOCOL.md section 15.2: under a format this client does not implement it takes min of
// each declared resource and writes nothing; other clients count its locks.
func TestTokenOnlyUnderUnknownFormat(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 3))
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 4, 7)).stop() // create every token file
	writeFormat(t, dir, "coreslot-protocol 2\n")
	before := listing(t, dir)

	tk, err := e.EnqueueContext(context.Background(), need("cpu=2..6"))
	if err != nil {
		t.Fatal(err)
	}
	defer tk.Withdraw()
	if !tk.TokenOnly() || tk.Name() != "" {
		t.Fatalf("token-only %v, ticket %q: want token-only without a ticket", tk.TokenOnly(), tk.Name())
	}
	g := admitted(t, step(t, tk))
	if !g.TokenOnly() || g.Grant()["cpu"] != 2 {
		t.Errorf("token-only %v grant %v, want token-only with min 2", g.TokenOnly(), g.Grant())
	}
	if files := g.Files(); len(files) != 3 || files[0] != nil {
		t.Errorf("files %v, want nil then 2 tokens", files)
	}
	env := strings.Join(g.Env(), " ")
	if env != "CORESLOT_GRANTED_CPU=2 CORESLOT_TOKEN_FDS=4-5 CORESLOT_UNBUDGETED=format" {
		t.Errorf("env %q", env)
	}
	if after := listing(t, dir); after != before {
		t.Errorf("token-only admission wrote to the state directory:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	other := newEngine(t, dir, nil)
	if n := lockedCount(t, other, "cpu"); n != 6 {
		t.Errorf("another client counts %d locked, want 6", n)
	}
	if free, err := other.Free("cpu"); err != nil || free != 2 {
		t.Errorf("Free = %d, %v; want 2", free, err)
	}
	for _, d := range []string{store.QueueDir, store.HoldsDir, store.CancelDir, store.LogDir} {
		if exists(filepath.Join(dir, d)) {
			t.Errorf("%s/ exists under an unknown format", d)
		}
	}
}

// PROTOCOL.md section 15.2: a token-only client that finds a format it implements enqueues instead
// of bypassing the queue.
func TestTokenOnlyEnqueuesWhenFormatIsUnderstood(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 7))
	writeFormat(t, dir, "coreslot-protocol 2\n")
	tk, err := e.EnqueueContext(context.Background(), need("cpu=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer tk.Withdraw()
	step(t, tk)
	if !tk.TokenOnly() {
		t.Fatal("not token-only under format 2")
	}
	if err := os.Remove(filepath.Join(dir, store.FormatFile)); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second)
	step(t, tk)
	if tk.TokenOnly() || !exists(filepath.Join(dir, store.QueueDir, tk.Name())) {
		t.Fatalf("token-only %v, ticket %q: want a queued ticket once FORMAT is absent", tk.TokenOnly(), tk.Name())
	}
	if body, _ := os.ReadFile(filepath.Join(dir, store.FormatFile)); string(body) != "coreslot-protocol 1\n" {
		t.Errorf("FORMAT %q, want format 1 written", body)
	}
}

// PROTOCOL.md section 15.2: a waiter that finds the format changed withdraws and continues in
// token-only mode, writing nothing.
func TestFormatChangeWhileWaitingGoesTokenOnly(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	lock := startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 7))
	tk := enqueue(t, e, need("cpu=1"))
	name := tk.Name()
	step(t, tk)
	writeFormat(t, dir, "coreslot-protocol 2\n")
	clk.advance(time.Second)
	step(t, tk)
	if !tk.TokenOnly() {
		t.Fatal("still queued under format 2")
	}
	if dead, err := e.st.Probe(store.QueueDir, name); err != nil || !dead {
		t.Errorf("the old ticket: dead %v, %v; want withdrawn (dead) and left in place", dead, err)
	}
	lock.stop()
	clk.advance(time.Second)
	if g := admitted(t, step(t, tk)); !g.TokenOnly() {
		t.Error("admitted, but not token-only")
	}
}

func TestFreeCountsAllLocks(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 12\n")
	e := newEngine(t, dir, newFakeClock())
	startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 10, 13)) // above the size after a shrink
	startHelper(t, "lock", dir, filepath.Join(dir, store.ResDir, "cpu", "007"))
	if free, err := e.Free("cpu"); err != nil || free != 8 {
		t.Errorf("Free = %d, %v; want 12 - 4 = 8 (007 is not a token)", free, err)
	}
	if _, err := e.Free("gpu"); err == nil {
		t.Error("Free of an undeclared resource: want an error")
	}
}

// E1, E2, E7: a fail-open hold is published only into an understood format, never with the
// reason "format", and removed like any hold.
func TestFailOpenHold(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	if _, err := e.FailOpen(need("cpu=2"), "format"); err == nil {
		t.Error(`FailOpen with the reason "format": want an error`)
	}
	g, err := e.FailOpen(need("cpu=2"), "configuration invalid")
	if err != nil {
		t.Fatal(err)
	}
	h := readHold(t, e, g)
	if !h.Unbudgeted || len(h.Grant) != 0 || !reflect.DeepEqual(h.UnbudgetedResources, []string{"cpu"}) {
		t.Errorf("hold %+v, want unbudgeted, empty grant, unbudgeted_resources [cpu]", h)
	}
	env := strings.Join(g.Env(), " ")
	if env != "CORESLOT_GRANTED_CPU=2 CORESLOT_HELD_FD=3 CORESLOT_UNBUDGETED=configuration invalid" {
		t.Errorf("env %q", env)
	}
	if rec, _ := e.st.Degraded(); rec == nil || rec.Reason != "configuration invalid" {
		t.Errorf("degraded %+v, want the reason recorded", rec)
	}
	before := readGen(t, e)
	g.Close()
	if entries, _, _ := e.st.List(store.HoldsDir); len(entries) != 0 || readGen(t, e) != before+1 {
		t.Errorf("after Close: holds %v, gen %d -> %d; want removed with one bump", entries, before, readGen(t, e))
	}

	for _, format := range []string{"coreslot-protocol 2\n", "coreslot-protocol x\n"} {
		writeFormat(t, dir, format)
		before := listing(t, dir)
		g, err := e.FailOpen(need("cpu=2"), "state error")
		if err == nil || g == nil || g.HoldName() != "" {
			t.Errorf("FORMAT %q: grant %v err %v, want a hold-less grant and an error", format, g, err)
		}
		if after := listing(t, dir); after != before {
			t.Errorf("FORMAT %q: a fail-open run wrote to the state directory", format)
		}
	}
}

// Format 0 is well-formed and undefined (PROTOCOL.md section 7.6): Snapshot reports it present
// and not understood, so that no reader takes it for an absent FORMAT, which also has Format 0.
// Mutation: drop the FormatPresent assignment in Snapshot and the format-0 case turns red.
func TestSnapshotTellsFormatZeroFromAbsent(t *testing.T) {
	for _, c := range []struct {
		name, body          string // body "" leaves FORMAT absent
		format              int
		present, understood bool
		malformed           bool
	}{
		{"absent", "", 0, false, true, false},
		{"format 0", "coreslot-protocol 0\n", 0, true, false, false},
		{"format 00", "coreslot-protocol 00", 0, true, false, false},
		{"format 1", "coreslot-protocol 1\n", 1, true, true, false},
		{"format 2", "coreslot-protocol 2\n", 2, true, false, false},
		{"malformed", "coreslot-protocol x\n", 0, true, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := newStateDir(t, "")
			if c.body != "" {
				writeFormat(t, dir, c.body)
			}
			r, err := newEngine(t, dir, nil).Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if r.Format != c.format || r.FormatPresent != c.present || r.FormatUnderstood != c.understood || (r.FormatError != "") != c.malformed {
				t.Errorf("format %d present %v understood %v error %q; want %d, %v, %v, malformed %v",
					r.Format, r.FormatPresent, r.FormatUnderstood, r.FormatError, c.format, c.present, c.understood, c.malformed)
			}
		})
	}
}

func TestSnapshotReadsWithoutWriting(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	g := admitted(t, step(t, enqueue(t, e, needLabel("cpu=3", "gate:\nunit"))))
	waiter := enqueue(t, e, needLabel("cpu=8", "ci"))
	startHelper(t, "section", dir, "")
	before := listing(t, dir)
	r, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if after := listing(t, dir); after != before {
		t.Error("Snapshot wrote to the state directory")
	}
	if !r.Section.Held || r.Section.PID == 0 {
		t.Errorf("section %+v, want held by the helper", r.Section)
	}
	if len(r.Holds) != 1 || r.Holds[0].Label != "gate:\nunit" || r.Holds[0].Grant["cpu"] != 3 || r.Holds[0].Name != g.HoldName() {
		t.Errorf("holds %+v", r.Holds)
	}
	if len(r.Tickets) != 1 || r.Tickets[0].Name != waiter.Name() || r.Tickets[0].Dead {
		t.Errorf("tickets %+v", r.Tickets)
	}
	if ri := r.Resources["cpu"]; ri.Size != testCPUs || ri.Locked != 3 || ri.Available != 5 {
		t.Errorf("cpu %+v, want size 8, locked 3, free 5", ri)
	}
	if r.Format != 1 || !r.FormatUnderstood {
		t.Errorf("format %d understood %v", r.Format, r.FormatUnderstood)
	}
}

// liveHoldIndices lists, per live hold, the (resource, index) pairs its body lists.
func liveHoldIndices(t *testing.T, e *Engine) map[string][]string {
	t.Helper()
	entries, _, err := e.st.List(store.HoldsDir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, en := range entries {
		dead, err := e.st.Probe(store.HoldsDir, en.Name)
		if err != nil || dead {
			continue
		}
		body, err := e.readBody(store.HoldsDir, en.Name)
		if err != nil {
			t.Fatal(err)
		}
		h, err := wire.DecodeHold(body)
		if err != nil {
			t.Fatal(err)
		}
		for res, idx := range h.Grant {
			for _, i := range idx {
				out[en.Name] = append(out[en.Name], fmt.Sprintf("%s/%d", res, i))
			}
		}
	}
	return out
}

// PROTOCOL.md section 12.3: Close releases the hold before the tokens, so that no live hold
// lists a token nobody locks; a head admitted in between can never publish a second live hold
// over the same indices (I1).
func TestCloseReleasesHoldBeforeTokens(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 2\n")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	g := admitted(t, step(t, enqueue(t, e, need("cpu=2"))))
	w := enqueue(t, e, need("cpu=2"))
	if isAdmitted(step(t, w)) {
		t.Fatal("admitted while the grant holds every token")
	}

	reached, release := make(chan struct{}), make(chan struct{})
	defaultPoint := point
	point = func(name string) {
		if name == "grant.after-hold-close" {
			close(reached)
			<-release
		}
	}
	closed := make(chan struct{})
	go func() {
		g.Close()
		close(closed)
	}()
	<-reached // paused between the two closes (a SIGSTOP there lasts indefinitely)
	clk.advance(time.Second)
	r, err := w.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for hold, idx := range liveHoldIndices(t, e) {
		for _, i := range idx {
			if other, dup := seen[i]; dup {
				t.Errorf("I1: live holds %s and %s both list %s", other, hold, i)
			}
			seen[i] = hold
		}
	}
	close(release)
	<-closed
	point = defaultPoint
	if a, ok := r.(Admitted); ok {
		a.Close()
		t.Fatal("admitted between the closes")
	}
	clk.advance(time.Second)
	if !isAdmitted(step(t, w)) {
		t.Error("not admitted once the grant is fully closed")
	}
}

// A ticket published ahead of the head between its election and its admitting section wins:
// the section repeats the election over a fresh listing, where a ticket the head has never
// watched counts as live (PROTOCOL.md section 12.1 step 5).
func TestReelectionInsideTheSection(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 1\n")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	h := enqueue(t, e, need("cpu=1"))
	var p *Ticket
	defaultPoint := point
	point = func(name string) {
		if name == "admit.before-enter" && p == nil {
			req := needLabel("cpu=1", "p")
			req.Priority = 10 // ahead of h
			p = enqueue(t, newEngine(t, dir, clk), req)
		}
	}
	t.Cleanup(func() { point = defaultPoint })
	if r := step(t, h); isAdmitted(r) {
		t.Fatal("h was admitted past p, published after h's election but before its section")
	}
	if p == nil || !p.key.Less(h.key) {
		t.Fatal("p was not published ahead of h")
	}
	clk.advance(time.Second)
	if pos := position(t, step(t, h)); pos != 2 {
		t.Fatalf("h at position %d behind the live p, want 2", pos)
	}
	if r := step(t, p); !isAdmitted(r) {
		t.Fatalf("p: %#v, want admitted", r)
	}
}

// A live ticket published ahead without a gen bump (a non-conforming writer) is first seen by
// the re-election, which keeps the head out; the next election rescans, so the head watches it
// and, as it is never refreshed, passes it over once it is STALLED instead of being kept out
// for ever.
func TestReelectionWatchesATicketPublishedWithoutBump(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 1\n")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	holder := startHelper(t, "lock", dir, tokenPaths(dir, "cpu", 0, 0))
	h := enqueue(t, e, need("cpu=1"))
	if pos := position(t, step(t, h)); pos != 1 { // elected, gen known; the token is held
		t.Fatalf("h at position %d, want the head", pos)
	}
	// x: a well-formed, live ticket ahead of h, published without bumping gen.
	body, err := wire.Encode(wire.Ticket{Label: "x", Need: need("cpu=1").Need, CreatedNS: 1})
	if err != nil {
		t.Fatal(err)
	}
	x := filepath.Join(dir, store.QueueDir, wire.TicketName(wire.ID{OrderKey: 1, PID: 1, Seq: 1}, "x"))
	if err := os.WriteFile(x, body, 0o600); err != nil {
		t.Fatal(err)
	}
	startHelper(t, "lock", dir, x)
	holder.stop() // the token is free
	clk.advance(time.Second)
	if r := step(t, h); isAdmitted(r) {
		t.Fatal("h was admitted past x, live and first seen in the section")
	}
	admittedAt := -1
	for sec := 1; sec <= 15 && admittedAt < 0; sec++ {
		clk.advance(time.Second)
		if isAdmitted(step(t, h)) {
			admittedAt = sec
		}
	}
	if admittedAt < 10 || admittedAt > 11 {
		t.Fatalf("h admitted %d s after the re-election first saw x, want 10 or 11 (x watched until STALLED)", admittedAt)
	}
}
