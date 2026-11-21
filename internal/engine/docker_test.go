//go:build unix

package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// ---------------------------------------------------------------- fixtures

// fakeDocker is a DockerSource whose next reading the test sets; it counts the readings. A reading
// whose StartedNS is 0 is stamped with the engine's clock when it is taken.
type fakeDocker struct {
	clk     *fakeClock
	reading DockerReading
	reads   int
	started int64  // the last reading's StartedNS
	onRead  func() // runs inside each Read
}

func (f *fakeDocker) source() DockerSource {
	return DockerSource{Read: func(context.Context) DockerReading {
		f.reads++
		if f.onRead != nil {
			f.onRead()
		}
		r := f.reading
		if r.StartedNS == 0 {
			r.StartedNS = f.clk.clock().Realtime()
		}
		f.started = r.StartedNS
		return r
	}}
}

// testVMMiB is the VM of the readings vm returns; its Docker reserve is 3072 MiB.
const testVMMiB = 16000

// vm is a reading of a full-grown VM whose estimate leaves roomMiB once the Docker reserve is taken.
func vm(roomMiB int64) DockerReading {
	return DockerReading{OK: true, VMMiB: testVMMiB, FreeEstMiB: roomMiB + state.DockerReserve(testVMMiB), VMNowMiB: testVMMiB}
}

func gated(mib, dockerMiB int64) spec.Request {
	return spec.Request{Label: "g", NeedMiB: mib, DockerNeedMiB: dockerMiB}
}

// gatedContainer is a gated container command with no pin: a container request.
func gatedContainer(dockerMiB int64) spec.Request {
	return spec.Request{Label: "c", NeedMiB: 1, RealMemory: true, DockerNeedMiB: dockerMiB}
}

func enqueueGated(t *testing.T, e *Engine, req spec.Request, f *fakeDocker, opts ...EnqueueOption) *Ticket {
	t.Helper()
	tk, err := e.EnqueueContext(context.Background(), req, append([]EnqueueOption{WithDocker(f.source())}, opts...)...)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	t.Cleanup(func() { _ = tk.Withdraw() })
	return tk
}

// writeHold writes h into holds/ under its own name, as another client's hold, and returns its
// path; it is live while something locks it (lockHere, or the lock helper).
func writeHold(t *testing.T, dir string, h wire.Hold) string {
	t.Helper()
	if h.PID == 0 {
		h.PID = 4000 + int(otherHolds.Add(1))
	}
	if h.StartNS == 0 {
		h.StartNS = 1
	}
	return writeEntry(t, dir, store.HoldsDir, wire.HoldName(h.StartNS, h.PID, 1, h.Label), h)
}

// writeTicket writes tk into queue/ as another client's ticket named by id, and returns its path
// and name; it is live while something locks it.
func writeTicket(t *testing.T, dir string, id wire.ID, tk wire.Ticket) (path, name string) {
	t.Helper()
	name = wire.TicketName(id, tk.Label)
	return writeEntry(t, dir, store.QueueDir, name, tk), name
}

func writeEntry(t *testing.T, dir, sub, name string, v any) string {
	t.Helper()
	body, err := wire.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, sub, name)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// lockHere locks path through a description of its own until the test ends, so that the entry is
// live to every probe.
func lockHere(t *testing.T, path string) {
	t.Helper()
	f, err := store.OpenFile(path, os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockNB(f); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
}

func hasNote(dir string, tk *Ticket) bool {
	return exists(filepath.Join(dir, store.VMWaitDir, tk.Name()))
}

// ---------------------------------------------------------------- the gated attempt

// A gated head that is short of the host makes no reading and has no note: the Mac test needs no
// reading. Mutation: skipping the room test for a gated ticket reads and admits it.
func TestMacTestFailsNoReading(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	lockHere(t, holdPath(t, dir, 1024)) // the whole room
	f := &fakeDocker{clk: clk, reading: vm(10000)}
	tk := enqueueGated(t, e, gated(128, 1000), f)
	wantReason(t, waitOutcome(t, tk), ReasonRoomShort, "")
	if f.reads != 0 || hasNote(dir, tk) {
		t.Errorf("%d readings, note %v; want none of either", f.reads, hasNote(dir, tk))
	}
}

// A gated container request leaves the VM growth the live holds reserve on the host; an ungated
// one waits on real memory alone. Mutation: leaving the holds out of the gated container request's
// room admits it.
func TestGatedContainerRequestRoomLeavesGrowth(t *testing.T) {
	dir := capped(t, 4000)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	lockHere(t, writeHold(t, dir, wire.Hold{Label: "grown", NeedMiB: 1, FootprintMiB: 1, DockerNeedMiB: 1500, DockerGrowthMiB: 3000}))
	f := &fakeDocker{clk: clk, reading: vm(10000)}
	c := enqueueGated(t, e, spec.Request{Label: "c", NeedMiB: 1500, RealMemory: true, DockerNeedMiB: 500}, f)
	w := waitOutcome(t, c)
	wantReason(t, w, ReasonRoomShort, "")
	if w.Outcome.RoomMiB != 1000 || f.reads != 0 {
		t.Errorf("room %d MiB with %d readings, want 1000 and none", w.Outcome.RoomMiB, f.reads)
	}
	admitted(t, step(t, enqueue(t, e, spec.Request{Label: "u", NeedMiB: 1500, RealMemory: true})))
}

// The reading is taken outside the section: when Read runs, another description can enter it.
// Mutation: taking the reading before leaving the section makes TryEnter fail.
func TestReadOutsideSection(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	f := &fakeDocker{clk: clk, reading: vm(10000)}
	f.onRead = func() {
		sec, ok, err := e.st.TryEnter()
		if err != nil || !ok {
			t.Errorf("Read ran while the section was held (%v)", err)
			return
		}
		sec.Leave()
	}
	admitted(t, step(t, enqueueGated(t, e, gated(128, 1000), f)))
	if f.reads != 1 {
		t.Errorf("%d readings, want 1", f.reads)
	}
}

// One Step of a gated container request is one attempt: the Mac test, the reading outside, and
// the entry after it, which makes the VM test and admits. Mutation: counting each entry as an
// attempt makes it two.
func TestReEntryIsSameAttempt(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	lockHere(t, holdPath(t, dir, 128))
	enqueue(t, e, need(1024)) // a live head that does not fit
	f := &fakeDocker{clk: clk, reading: vm(10000)}
	c := enqueueGated(t, e, gatedContainer(1000), f)
	enters, tries := countEntries(t)
	admitted(t, step(t, c))
	if c.attempts != 1 || f.reads != 1 || *enters+*tries != 2 {
		t.Errorf("%d attempts, %d readings, %d entries; want 1, 1 and 2", c.attempts, f.reads, *enters+*tries)
	}
}

// A reading taken before a gated hold's docker_sampled_ns is not used: the attempt reads again at
// once, and when that reading is rejected too the attempt ends; the next attempt reads again.
// Mutation: ending the attempt at the first rejection makes one reading where two are wanted.
func TestStampRuleRereadsOnce(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	sampled := clk.clock().Realtime() + int64(time.Second)
	lockHere(t, writeHold(t, dir, wire.Hold{Label: "gated", NeedMiB: 128, DockerNeedMiB: 500, DockerSampledNS: sampled}))
	r := vm(10000)
	r.StartedNS = sampled - 1
	f := &fakeDocker{clk: clk, reading: r}
	tk := enqueueGated(t, e, gated(128, 1000), f)
	if w := waitOutcome(t, tk); f.reads != 2 {
		t.Fatalf("%d readings, want 2 (%+v)", f.reads, w)
	}
	clk.advance(time.Second)
	f.reading.StartedNS = 0 // from now on, the clock: as late as the hold's sample
	admitted(t, step(t, tk))
	if f.reads != 3 {
		t.Errorf("%d readings, want 3: the reading in hand is rejected, the re-read is used", f.reads)
	}
}

// A candidate leaves the earlier gated tickets their stated Docker needs: with 7000 MiB of VM room
// and 6000 asked for ahead, 1000 fits and 2000 waits with a note. Mutation: dropping the sum of
// the gated tickets ahead admits the 2000.
func TestVMTestSumsGatedAhead(t *testing.T) {
	for _, tc := range []struct {
		dockerMiB int64
		admit     bool
	}{{1000, true}, {2000, false}} {
		dir := newStateDir(t, "")
		clk := newFakeClock()
		e := newEngine(t, dir, clk)
		lockHere(t, holdPath(t, dir, 128))
		f := &fakeDocker{clk: clk, reading: vm(7000)}
		enqueueGated(t, e, gated(128, 4000), f) // never stepped: live, and gated
		enqueueGated(t, e, gated(128, 2000), f)
		c := enqueueGated(t, e, gatedContainer(tc.dockerMiB), f)
		r := step(t, c)
		if isAdmitted(r) != tc.admit {
			t.Fatalf("Docker need %d: admitted %v, want %v (%#v)", tc.dockerMiB, isAdmitted(r), tc.admit, r)
		}
		if !tc.admit {
			wantReason(t, r.(Waiting), ReasonVMRoomShort, "")
			if !hasNote(dir, c) {
				t.Error("a candidate short of VM room has no note")
			}
		}
	}
}

// The sum leaves out a dead gated ticket ahead and one whose Docker need is above V less the
// Docker reserve by this reading, at once, and a STALLED one once watched for the stall time.
// Mutations: summing the dead one or the oversized one keeps the first candidate out; summing the
// STALLED one keeps the second out at 10 s.
func TestVMTestLeavesOutDeadStalledOversized(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	lockHere(t, holdPath(t, dir, 128))
	f := &fakeDocker{clk: clk, reading: vm(6000)} // the candidate and the stopped ticket, no more
	c := enqueueGated(t, e, gatedContainer(1000), f)
	// Written after the candidate's enqueue, whose reconciliation would remove the dead one.
	for i, dockerMiB := range []int64{5000, 5000, testVMMiB - state.DockerReserve(testVMMiB) + 1} {
		p, _ := writeTicket(t, dir, wire.ID{OrderKey: uint64(i + 1), PID: 4100 + i, Seq: 1},
			wire.Ticket{Label: "ahead", NeedMiB: 128, CreatedNS: int64(i + 1), DockerNeedMiB: dockerMiB})
		if i > 0 { // the first is dead; the second never refreshes; the third is oversized
			lockHere(t, p)
		}
	}
	admitted(t, step(t, c))

	f.reading = vm(2000) // the first candidate's hold takes 1000 of it
	c2 := enqueueGated(t, e, gatedContainer(1000), f)
	for sec := 0; sec <= 10; sec++ {
		r := step(t, c2)
		if sec < 10 && isAdmitted(r) {
			t.Fatalf("%d s: admitted while the stopped ticket still counts", sec)
		}
		if sec == 10 && !isAdmitted(r) {
			t.Fatalf("10 s: want admitted past the STALLED ticket, got %#v", r)
		}
		clk.advance(time.Second)
	}
}

// A dead ticket with a note is left out as dead: its note does not make it count. Mutation: summing
// a dead ticket keeps the candidate out.
func TestDeadNotedTicketSkippedAsDead(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	lockHere(t, holdPath(t, dir, 128))
	f := &fakeDocker{clk: clk, reading: vm(1000)}
	c := enqueueGated(t, e, gatedContainer(1000), f)
	_, name := writeTicket(t, dir, wire.ID{OrderKey: 1, PID: 4100, Seq: 1},
		wire.Ticket{Label: "ahead", NeedMiB: 128, CreatedNS: 1, DockerNeedMiB: 5000}) // never locked: dead
	note(t, e, name)
	admitted(t, step(t, c))
}

// A gated waiter observes every gated ticket ahead at each election, beyond the front window too,
// so one that stopped stops counting once it has been watched for the stall time, whether or not
// the waiter reached its VM test meanwhile; one it has never observed counts. Mutation: watching
// only the front window leaves the stopped ticket unobserved, so it counts at 10 s.
func TestGatedWaiterObservesEveryGatedTicketAhead(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	lockHere(t, holdPath(t, dir, 1)) // a hold live: the container request needs no re-election
	// No host room for a gated container request while this hold reserves growth.
	grown := startHelper(t, "lock", dir, writeHold(t, dir, wire.Hold{Label: "grown", NeedMiB: 1, FootprintMiB: 1, DockerGrowthMiB: 2000}))
	var live []*helper // the front window, then the stopped gated ticket, then the nearest ticket ahead
	for i := 0; i < FrontWindow; i++ {
		live = append(live, startHelper(t, "enqueue", dir, "1024"))
	}
	now := time.Now().UnixNano()
	p, _ := writeTicket(t, dir, wire.ID{OrderKey: uint64(now), PID: 4200, Seq: 1},
		wire.Ticket{Label: "stopped", NeedMiB: 128, CreatedNS: now, DockerNeedMiB: 4000})
	lockHere(t, p)
	live = append(live, startHelper(t, "enqueue", dir, "1024"))
	f := &fakeDocker{clk: clk, reading: vm(1000)}
	c := enqueueGated(t, e, gatedContainer(1000), f)

	sec, err := e.st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sum, err := c.gatedAheadMiB(testVMMiB - state.DockerReserve(testVMMiB))
	sec.Leave()
	if err != nil || sum != 4000 {
		t.Fatalf("never observed: the stopped ticket's need sums to %d (%v), want 4000", sum, err)
	}

	for s := 0; s < 10; s++ {
		for _, h := range live {
			h.command(t, "refresh")
		}
		if r := step(t, c); isAdmitted(r) || f.reads != 0 {
			t.Fatalf("%d s: %#v with %d readings while the host is short", s, r, f.reads)
		}
		clk.advance(time.Second)
	}
	grown.kill(t)
	for _, h := range live {
		h.command(t, "refresh")
	}
	if r := step(t, c); !isAdmitted(r) {
		t.Fatalf("the stopped gated ticket was watched for 10 s: want admitted, got %#v", r)
	}
}

// The admitted hold carries its Docker need, the growth the admitting reading gives from the
// request's footprint, and that reading's stamp, in the body it is published with. Mutation:
// leaving the footprint out of the growth makes it 2000.
func TestAdmittedHoldCarriesGrowthInFirstBody(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	f := &fakeDocker{clk: clk, reading: DockerReading{OK: true, VMMiB: testVMMiB, FreeEstMiB: 10000, VMNowMiB: 10000}}
	req := gated(128, 1000)
	req.DockerFootprintMiB = 300
	g := admitted(t, step(t, enqueueGated(t, e, req, f)))
	h := readHold(t, e, g)
	if h.DockerNeedMiB != 1000 || h.DockerGrowthMiB != 1400 || h.DockerSampledNS != f.started {
		t.Errorf("hold need %d, growth %d, sampled %d; want 1000, 1400, %d", h.DockerNeedMiB, h.DockerGrowthMiB,
			h.DockerSampledNS, f.started)
	}
	if !g.DockerGated() || g.DockerVMMiB() != testVMMiB || g.DockerUngated() != "" {
		t.Errorf("grant gated %v, V %d, ungated %q", g.DockerGated(), g.DockerVMMiB(), g.DockerUngated())
	}
}

// The level gate records a start for spacing only when the attempt admits, not when the Mac test
// alone passed. Mutation: recording when the level gate is checked writes it at the VM failure.
func TestLevelGateRecordsOnlyOnAdmission(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	hold(t, e, need(128))
	setWatch(t, e, clk, watch.Detector{YellowClearedNS: sampleWallNS - 1e9}, 0)
	f := &fakeDocker{clk: clk, reading: vm(500)}
	tk := enqueueGated(t, e, gated(128, 1000), f)
	wantReason(t, waitOutcome(t, tk), ReasonVMRoomShort, "")
	if d, _, _, _ := e.st.ReadWatch(); d.LastAdmitNS != 0 {
		t.Fatalf("a VM failure recorded a start: %d", d.LastAdmitNS)
	}
	f.reading = vm(5000)
	clk.advance(ReadEvery)
	admitted(t, step(t, tk))
	if d, _, _, _ := e.st.ReadWatch(); d.LastAdmitNS == 0 {
		t.Error("the admission recorded no start")
	}
}

// A VM test that fails publishes the note; the admission removes it with the ticket. Mutation:
// publishing no note leaves the first check without one.
func TestVMTestFailPublishesNote(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	f := &fakeDocker{clk: clk, reading: vm(500)}
	tk := enqueueGated(t, e, gated(128, 1000), f)
	w := waitOutcome(t, tk)
	wantReason(t, w, ReasonVMRoomShort, "")
	if !w.Outcome.Noted || !hasNote(dir, tk) {
		t.Errorf("noted %v, note %v; want both", w.Outcome.Noted, hasNote(dir, tk))
	}
}

// Mutation: leaving the note out of the removal of one's own ticket keeps it after the admission.
func TestAdmissionRemovesNote(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	f := &fakeDocker{clk: clk, reading: vm(500)}
	tk := enqueueGated(t, e, gated(128, 1000), f)
	waitOutcome(t, tk)
	f.reading = vm(5000)
	clk.advance(ReadEvery)
	admitted(t, step(t, tk))
	if hasNote(dir, tk) {
		t.Error("the admitted ticket's note is still there")
	}
}

// An attempt whose Mac test fails removes the note at once, within 2 s of its publication.
// Mutation: keeping the note when the Mac test fails.
func TestMacFailRemovesNoteAtOnce(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	f := &fakeDocker{clk: clk, reading: vm(500)}
	tk := enqueueGated(t, e, gated(128, 1000), f)
	waitOutcome(t, tk)
	lockHere(t, holdPath(t, dir, 1024)) // the host's room is gone
	clk.advance(500 * time.Millisecond)
	w := waitOutcome(t, tk)
	wantReason(t, w, ReasonRoomShort, "")
	if w.Outcome.Noted || hasNote(dir, tk) {
		t.Error("the note stayed although the Mac test failed")
	}
}

// No reading happens between a lock hook's enter and its leave: not inside the section, whose
// entry and leave the hooks bracket, nor inside any probe. Mutation: taking the reading before
// leaving the section.
func TestNoDockerCallInsideSection(t *testing.T) {
	var depth atomic.Int64
	store.SetLockHooks(func() { depth.Add(1) }, func() { depth.Add(-1) })
	t.Cleanup(func() { store.SetLockHooks(func() {}, func() {}) })
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	sampled := clk.clock().Realtime() + int64(time.Second)
	lockHere(t, writeHold(t, dir, wire.Hold{Label: "gated", NeedMiB: 128, DockerNeedMiB: 500, DockerSampledNS: sampled}))
	f := &fakeDocker{clk: clk, reading: vm(500)}
	f.onRead = func() {
		if d := depth.Load(); d != 0 {
			t.Errorf("Read inside %d lock brackets", d)
		}
	}
	tk := enqueueGated(t, e, gated(128, 1000), f)
	waitOutcome(t, tk) // two readings, both before the hold's sample: the stamp rule rejects them
	clk.advance(ReadEvery)
	f.reading = vm(5000)
	admitted(t, step(t, tk))
	if f.reads != 3 {
		t.Errorf("%d readings, want 3", f.reads)
	}
}
