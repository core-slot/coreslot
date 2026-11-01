//go:build unix

package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
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

func admitted(t *testing.T, r StepResult) *Grant {
	t.Helper()
	a, ok := r.(Admitted)
	if !ok {
		t.Fatalf("want Admitted, got %#v", r)
	}
	return a.Grant
}

// A hold carries its reservation in its body: a held grant's hold says need_mib, the reservation
// keeps every other request out, and killing the holder frees it at the next attempt. A grant
// rewrites its own body in place, keeping the inode and the lock. Mutations: summing nothing in
// reservedMiB admits the waiter beside the helper's hold; replacing the file in RewriteHold
// loses the lock.
func TestHoldIsTheReservation(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	holder := startHelper(t, "hold", dir, "mem=8")
	body, err := e.readBody(store.HoldsDir, holder.detail)
	if err != nil {
		t.Fatal(err)
	}
	if h, err := wire.DecodeHold(body); err != nil || h.NeedMiB != testUnits*unit || h.PID != holder.cmd.Process.Pid {
		t.Fatalf("the helper's hold %+v, %v; want need_mib %d of pid %d", h, err, testUnits*unit, holder.cmd.Process.Pid)
	}
	tk := enqueue(t, e, need("mem=1"))
	if isAdmitted(step(t, tk)) {
		t.Fatal("admitted beside a hold that reserves the whole budget")
	}
	holder.kill(t)
	clk.advance(time.Second)
	g := admitted(t, step(t, tk))
	if h := readHold(t, e, g); h.NeedMiB != unit || h.ArrivalNS != tk.arrivalNS || h.PID != os.Getpid() {
		t.Errorf("hold %+v, want need_mib %d, the ticket's arrival and this pid", h, unit)
	}

	path := filepath.Join(dir, store.HoldsDir, g.HoldName())
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateHold(func(h *wire.Hold) { h.NeedMiB, h.FootprintMiB, h.Label = 3*unit, 90, "renamed" }); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("UpdateHold replaced the hold's file: %v", err)
	}
	if h := readHold(t, e, g); h.NeedMiB != 3*unit || h.FootprintMiB != 90 || h.Label != "" {
		t.Errorf("hold after UpdateHold %+v, want need 3 units, footprint 90 and the label of its name", h)
	}
	if dead, err := e.st.Probe(store.HoldsDir, g.HoldName()); err != nil || dead {
		t.Errorf("the rewritten hold: dead %v, %v; want still locked", dead, err)
	}
	if n := reservedUnits(t, e); n != 3 {
		t.Errorf("reserved %d units after UpdateHold, want 3", n)
	}
	g.Close()
	if err := g.UpdateHold(func(*wire.Hold) {}); err == nil {
		t.Error("UpdateHold of a closed grant: want an error")
	}
	if entries, _, _ := e.st.List(store.HoldsDir); len(entries) != 1 { // the killed helper's, dead
		t.Errorf("after Close, holds %v", entries)
	}
}

// A need above the machine is representable, in the ticket and in the hold, with no cap: on a
// 32 GiB machine a 40 GiB need runs alone, as asked.
func TestNoPerGrantCap(t *testing.T) {
	dir := newStateDir(t, "")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := New(st, filepath.Join(dir, store.ConfigFile),
		func() (config.Machine, error) {
			return config.Machine{MemMiB: 32768, HeadroomMiB: 20000, HeadroomOK: true}, nil
		},
		newFakeClock().clock())
	if total, over := e.Oversized(40960); total != 32768 || !over {
		t.Errorf("Oversized(40960) = %d, %v; want 32768, true", total, over)
	}
	tk := enqueue(t, e, spec.Request{NeedMiB: 40960})
	body, err := e.readBody(store.QueueDir, tk.Name())
	if err != nil {
		t.Fatal(err)
	}
	if w, err := wire.DecodeTicket(body); err != nil || w.NeedMiB != 40960 {
		t.Fatalf("ticket %+v, %v; want need_mib 40960", w, err)
	}
	g := admitted(t, step(t, tk))
	if g.GrantMiB() != 40960 || readHold(t, e, g).NeedMiB != 40960 {
		t.Errorf("grant %d hold %+v; want 40960 and need_mib 40960", g.GrantMiB(), readHold(t, e, g))
	}
}

// A need above what the room can ever be waits for the machine to be idle, then runs alone, its
// hold reserving the need asked.
func TestOversizeRunsAlone(t *testing.T) {
	dir := capped(t, 1280)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	other := startHelper(t, "lock", dir, holdPath(t, dir, 1))
	tk := enqueue(t, e, need("mem=16"))
	if r := step(t, tk); isAdmitted(r) {
		t.Fatal("an oversized request admitted beside a live hold")
	}
	other.stop()
	clk.advance(time.Second)
	g := admitted(t, step(t, tk))
	if g.GrantMiB() != 16*unit {
		t.Errorf("grant %d, want %d", g.GrantMiB(), 16*unit)
	}
	if n := reservedUnits(t, e); n != 16 {
		t.Errorf("reserved %d units, want the 16 asked", n)
	}
}

// I4: a waiter behind a blocked head whose need is the whole budget takes nothing, through Step
// or Poll, and never tries admission: only the head does (PROTOCOL.md section 10.3).
func TestOnlyHeadTakesTheBudget(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	startHelper(t, "lock", dir, holdPath(t, dir, 1))
	head := enqueue(t, e, need("mem=8")) // cannot be admitted: 7 of 8 free
	behind := enqueue(t, e, need("mem=1"))
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
		if n := reservedUnits(t, e); n != 1 {
			t.Fatalf("round %d: %d units reserved, want only the helper's 1", round, n)
		}
	}
	if behind.attempts != 0 {
		t.Errorf("the waiter behind tried admission %d times", behind.attempts)
	}
}

// PROTOCOL.md section 13.1: a cancel published before the head's admitting section wins.
func TestCancelPublishedBeforeAdmissionWins(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	head := enqueue(t, e, need("mem=2"))

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
	if err := newEngine(t, dir, nil).CancelContext(context.Background(), ticketID(head)); err != nil { // its own completed section
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
	if n := reservedUnits(t, e); n != 0 {
		t.Errorf("%d units reserved by a cancelled head", n)
	}
	if exists(filepath.Join(dir, store.CancelDir, ticketID(head).String())) {
		t.Error("the cancel request was not removed")
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

// Under a format this client does not implement, an enqueue refuses and writes nothing, and so
// does a waiter that finds the format changed.
func TestUnknownFormatRefuses(t *testing.T) {
	refused := func(t *testing.T, err error) {
		t.Helper()
		var fe *store.FormatError
		if !errors.Is(err, ErrRefused) || !errors.As(err, &fe) || fe.N != 3 {
			t.Fatalf("%v, want a refusal naming format 3", err)
		}
	}
	t.Run("enqueue", func(t *testing.T) {
		dir := newStateDir(t, "")
		e := newEngine(t, dir, newFakeClock())
		writeFormat(t, dir, "coreslot-protocol 3\n")
		before := listing(t, dir)
		_, err := e.EnqueueContext(context.Background(), need("mem=1"))
		refused(t, err)
		if after := listing(t, dir); after != before {
			t.Errorf("a refused enqueue wrote to the state directory:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})
	t.Run("waiting", func(t *testing.T) {
		dir := newStateDir(t, "")
		clk := newFakeClock()
		e := newEngine(t, dir, clk)
		startHelper(t, "lock", dir, holdPath(t, dir, 8))
		tk := enqueue(t, e, need("mem=1"))
		step(t, tk)
		writeFormat(t, dir, "coreslot-protocol 3\n")
		clk.advance(time.Second)
		_, err := tk.Step(context.Background())
		refused(t, err)
	})
}

// E1, E2, E7: a fail-open hold is published only into an understood format and removed like any
// hold.
func TestFailOpenHold(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	g, err := e.FailOpen(need("mem=2"), "configuration invalid")
	if err != nil {
		t.Fatal(err)
	}
	if h := readHold(t, e, g); h.NeedMiB != 0 {
		t.Errorf("hold %+v, want need_mib 0: it reserves nothing", h)
	}
	env := strings.Join(g.Env(), " ")
	if env != "CORESLOT_GRANTED_MEM=256 CORESLOT_HOLD="+g.HoldName()+" CORESLOT_UNBUDGETED=configuration invalid" {
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

	for _, format := range []string{"coreslot-protocol 3\n", "coreslot-protocol x\n"} {
		writeFormat(t, dir, format)
		before := listing(t, dir)
		g, err := e.FailOpen(need("mem=2"), "state error")
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
		{"format 1", "coreslot-protocol 1\n", 1, true, false, false},
		{"format 2", "coreslot-protocol 2\n", 2, true, true, false},
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
	g := admitted(t, step(t, enqueue(t, e, needLabel("mem=3", "gate:\nunit"))))
	waiter := enqueue(t, e, needLabel("mem=8", "ci"))
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
	if len(r.Holds) != 1 || r.Holds[0].Label != "gate:\nunit" || r.Holds[0].NeedMiB != 3*unit || r.Holds[0].Name != g.HoldName() {
		t.Errorf("holds %+v", r.Holds)
	}
	if len(r.Tickets) != 1 || r.Tickets[0].Name != waiter.Name() || r.Tickets[0].Dead {
		t.Errorf("tickets %+v", r.Tickets)
	}
	if m := r.Mem; m.ReservedMiB != 3*unit || m.FreeMiB != 5*unit {
		t.Errorf("mem %+v, want reserved 384, free 640", m)
	}
	if r.Format != 2 || !r.FormatUnderstood {
		t.Errorf("format %d understood %v", r.Format, r.FormatUnderstood)
	}
}

// A live hold's owner exited when the process listing lacks its pid, or the process there started
// more than a second after the hold (the pid was reused); a listing that cannot be read reports
// nothing. Mutations: OwnerExited set from the pid's presence alone reddens "pid reused"; set
// regardless of the listing's ok reddens "listing unread".
func TestSnapshotOwnerExited(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, nil)
	admitted(t, step(t, enqueue(t, e, need("mem=2"))))
	r, err := e.Snapshot()
	if err != nil || len(r.Holds) != 1 {
		t.Fatalf("holds %+v, %v; want one", r.Holds, err)
	}
	h := r.Holds[0]
	saved := listProcs
	t.Cleanup(func() { listProcs = saved })
	for _, c := range []struct {
		name  string
		procs map[int]memsample.Proc
		ok    bool
		want  bool
	}{
		{"alive", map[int]memsample.Proc{h.PID: {StartNS: h.StartNS - int64(time.Hour)}}, true, false},
		{"alive within the slack", map[int]memsample.Proc{h.PID: {StartNS: h.StartNS + int64(time.Second)}}, true, false},
		{"pid absent", map[int]memsample.Proc{}, true, true},
		{"pid reused", map[int]memsample.Proc{h.PID: {StartNS: h.StartNS + int64(2*time.Second)}}, true, true},
		{"listing unread", nil, false, false},
	} {
		listProcs = func() (map[int]memsample.Proc, bool) { return c.procs, c.ok }
		r, err := e.Snapshot()
		if err != nil || len(r.Holds) != 1 || r.Holds[0].OwnerExited != c.want {
			t.Errorf("%s: holds %+v, %v; want OwnerExited %v", c.name, r.Holds, err, c.want)
		}
	}
}

// A ticket published ahead of the head between its election and its admitting section wins:
// the section repeats the election over a fresh listing, where a ticket the head has never
// watched counts as live (PROTOCOL.md section 12.1 step 5).
func TestReelectionInsideTheSection(t *testing.T) {
	dir := capped(t, 128)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	h := enqueue(t, e, need("mem=1"))
	var p *Ticket
	defaultPoint := point
	point = func(name string) {
		if name == "admit.before-enter" && p == nil {
			early := &fakeClock{mono: clk.clock().Monotonic(), real: clk.clock().Realtime() - int64(time.Minute)}
			p = enqueue(t, newEngine(t, dir, early), needLabel("mem=1", "p")) // an earlier arrival: ahead of h
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
	dir := capped(t, 128)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	holder := startHelper(t, "lock", dir, holdPath(t, dir, 1))
	h := enqueue(t, e, need("mem=1"))
	if pos := position(t, step(t, h)); pos != 1 { // elected, gen known; the budget is held
		t.Fatalf("h at position %d, want the head", pos)
	}
	// x: a well-formed, live ticket ahead of h, published without bumping gen.
	body, err := wire.Encode(wire.Ticket{Label: "x", NeedMiB: unit, CreatedNS: 1})
	if err != nil {
		t.Fatal(err)
	}
	x := filepath.Join(dir, store.QueueDir, wire.TicketName(wire.ID{OrderKey: 1, PID: 1, Seq: 1}, "x"))
	if err := os.WriteFile(x, body, 0o600); err != nil {
		t.Fatal(err)
	}
	startHelper(t, "lock", dir, x)
	holder.stop() // the budget is free
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
