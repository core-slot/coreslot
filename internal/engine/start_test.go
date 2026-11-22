//go:build unix

package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// requestStart publishes a start request for tk from another client and lets 10 ms pass: not even
// an election is due, so the check runs on every poll.
func requestStart(t *testing.T, dir string, clk *fakeClock, tk *Ticket) {
	t.Helper()
	if err := newEngine(t, dir, nil).StartContext(context.Background(), ticketID(tk)); err != nil {
		t.Fatalf("StartContext: %v", err)
	}
	clk.advance(10 * time.Millisecond)
}

// ownHold is the body of the live hold g published.
func ownHold(t *testing.T, e *Engine, g *Grant) wire.Hold {
	t.Helper()
	holds, _, _, err := holdListing(e.st, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range holds {
		if h.name == g.HoldName() {
			return h.body
		}
	}
	t.Fatalf("no live hold %s", g.HoldName())
	return wire.Hold{}
}

// A start request admits its ticket at once whatever the room and the level gate: its hold
// reserves the full need, keeps the ticket's arrival and says forced, and the room goes negative.
// The ticket, the request and the order override go, gen is bumped, and no spacing slot is
// recorded. Mutations: keeping the room test or honouring the level gate leaves it waiting;
// publishing less than the need fails the reservation; recording the gate's slot moves
// last_admit_ns; leaving StartDir out of RemoveOwnTicket keeps the request.
func TestStartNowAdmitsWhateverTheRoomAndTheGate(t *testing.T) {
	for _, c := range []struct {
		name string
		d    watch.Detector
	}{
		{"yellow", watch.Detector{Level: watch.Yellow}},
		{"cleared", watch.Detector{YellowClearedNS: sampleWallNS - 1e9}}, // an ordinary start records its slot
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := capped(t, 128)
			clk := newFakeClock()
			e := newEngine(t, dir, clk)
			hold(t, e, need(128)) // the whole room
			setWatch(t, e, clk, c.d, 0)
			tk := enqueue(t, e, need(512))
			wantReason(t, waitOutcome(t, tk), ReasonRoomShort, "")
			id := ticketID(tk)
			sec, err := e.st.Enter(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = sec.PublishOrder(id, []byte(`{"order_ns":7}`))
			sec.Leave()
			if err != nil {
				t.Fatal(err)
			}
			before, err := e.st.ReadGen()
			if err != nil {
				t.Fatal(err)
			}

			requestStart(t, dir, clk, tk)
			g := admitted(t, step(t, tk))
			defer g.Close()
			if p := g.Admission().Pass; p != "forced" {
				t.Errorf("pass %q, want forced", p)
			}
			if h := ownHold(t, e, g); h.NeedMiB != 512 || !h.Forced || h.ArrivalNS != tk.arrivalNS {
				t.Errorf("hold need %d, forced %v, arrival %d; want 512, true, the ticket's %d", h.NeedMiB, h.Forced, h.ArrivalNS, tk.arrivalNS)
			}
			rep, err := e.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if rep.Mem.RoomMiB >= 0 {
				t.Errorf("room %d after a forced start past it, want below 0", rep.Mem.RoomMiB)
			}
			for _, p := range []string{filepath.Join(dir, store.QueueDir, tk.Name()), filepath.Join(dir, store.StartDir, id.String()),
				filepath.Join(dir, store.OrderDir, id.String())} {
				if exists(p) {
					t.Errorf("%s outlived the forced admission", p)
				}
			}
			if after, err := e.st.ReadGen(); err != nil || after.N <= before.N {
				t.Errorf("gen %d after the admission, %v; want above %d", after.N, err, before.N)
			}
			if d, _, _, _ := e.st.ReadWatch(); d.LastAdmitNS != c.d.LastAdmitNS {
				t.Errorf("last_admit_ns %d after a forced start, want %d: no spacing slot", d.LastAdmitNS, c.d.LastAdmitNS)
			}
		})
	}
}

// A forced run is a red victim by its queue arrival, like any other run: a run that arrived after
// it is listed first. Mutation: stamping the forced start's time as its arrival lists it first.
func TestStartedRunIsAVictimByItsArrival(t *testing.T) {
	dir := capped(t, 128)
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	tk := enqueue(t, e, need(512))
	clk.advance(time.Second)
	later := wire.Hold{Label: "later", ArrivalNS: clk.clock().Realtime(), NeedMiB: 128}
	lockHere(t, writeHold(t, dir, later))
	clk.advance(10 * time.Second)
	requestStart(t, dir, clk, tk)
	g := admitted(t, step(t, tk))
	defer g.Close()

	now := clk.clock().Realtime()
	mark := func(h *wire.Hold) { h.Victim, h.Learned, h.SampledNS = true, true, now }
	if err := g.UpdateHold(mark); err != nil {
		t.Fatal(err)
	}
	holds, _, _, err := holdListing(e.st, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range holds {
		if holds[i].name != g.HoldName() {
			mark(&holds[i].body)
		}
	}
	stop, _ := watch.StopList(runningJobs(holds, now), false, 1, nil, 0)
	if len(stop) != 1 || stop[0] == g.HoldName() {
		t.Errorf("stop list %v; want the later arrival, not the forced run %s", stop, g.HoldName())
	}
}

// A cancel request beats a start request: Step checks it first, and the forced admission checks it
// again inside its section, as every admission does. Mutation: checking the start first and entering
// without that second check admits it (either alone still cancels).
func TestCancelBeatsStart(t *testing.T) {
	dir, e, clk, tk := blocked(t, 128)
	other := newEngine(t, dir, nil)
	if err := other.CancelContext(context.Background(), ticketID(tk)); err != nil {
		t.Fatal(err)
	}
	requestStart(t, dir, clk, tk)
	if r, err := tk.Step(context.Background()); !errors.Is(err, ErrCanceled) {
		if a, ok := r.(Admitted); ok {
			a.Close()
		}
		t.Fatalf("Step with both requests: %#v, %v; want ErrCanceled", r, err)
	}
	if n := reservedMiB(t, e); n != 128 {
		t.Errorf("%d MiB reserved, want only the helper's 128", n)
	}
	if exists(filepath.Join(dir, store.StartDir, ticketID(tk).String())) {
		t.Error("the start request outlived its cancelled ticket")
	}
}

// A waiter behind the head with no typical run time, which never attempts admission, still sees
// its start request. Mutation: checking after the early return leaves it waiting.
func TestStartReachesAWaiterBehindTheHead(t *testing.T) {
	dir, e, clk, head := blocked(t, 128)
	tk := enqueue(t, e, need(64))
	wantReason(t, waitOutcome(t, tk), ReasonBehindTicket, "")
	requestStart(t, dir, clk, tk)
	g := admitted(t, step(t, tk))
	defer g.Close()
	if isAdmitted(step(t, head)) {
		t.Error("the head was admitted beside the forced start")
	}
}

// A gated ticket short of Docker's VM starts on its request with its Docker keys, as a gated
// admission writes them, and its note goes. Mutation: publishing it without the reading drops the keys.
func TestStartNowKeepsDockerKeys(t *testing.T) {
	dir := newStateDir(t, "")
	clk := newFakeClock()
	e := newEngine(t, dir, clk)
	lockHere(t, holdPath(t, dir, 128))
	f := &fakeDocker{clk: clk, reading: DockerReading{OK: true, VMMiB: testVMMiB, FreeEstMiB: 0, VMNowMiB: 8000}}
	tk := enqueueGated(t, e, gated(128, 1000), f)
	wantReason(t, waitOutcome(t, tk), ReasonVMRoomShort, "")
	if !hasNote(dir, tk) {
		t.Fatal("no note for a ticket short of the VM")
	}
	clk.advance(ReadEvery)
	requestStart(t, dir, clk, tk)
	g := admitted(t, step(t, tk))
	defer g.Close()
	if h := ownHold(t, e, g); h.DockerNeedMiB != 1000 || h.DockerGrowthMiB != 2000 || h.DockerSampledNS != f.started || !h.Forced {
		t.Errorf("hold docker need %d, growth %d, sampled %d, forced %v; want 1000, 2000, %d, true",
			h.DockerNeedMiB, h.DockerGrowthMiB, h.DockerSampledNS, h.Forced, f.started)
	}
	if hasNote(dir, tk) {
		t.Error("the note outlived the forced admission")
	}
}
