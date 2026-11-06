//go:build unix

package engine

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// setLiveMem replaces the headroom testMachine reports, in MiB, for one test.
func setLiveMem(t *testing.T, headroomMiB int64) {
	t.Helper()
	saved := testHeadroom
	t.Cleanup(func() { testHeadroom = saved })
	testHeadroom.mib, testHeadroom.ok = headroomMiB, true
}

// The room is the headroom less the reserve and every live hold's unmeasured need: of 4096 MiB,
// 1024 are the reserve and A's 1 GiB, so B's 2 GiB fits exactly; C's does not. Mutation: forgetting
// the reserve admits C; comparing need < room refuses B.
func TestRoomGatesAdmission(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	setLiveMem(t, 4096)
	admitted(t, step(t, enqueue(t, e, need(1024))))
	admitted(t, step(t, enqueue(t, e, need(2048))))
	if r := step(t, enqueue(t, e, need(1))); isAdmitted(r) {
		t.Fatal("a request admitted with no room left")
	}
}

// With no hold live the head starts whatever the numbers say; the next request, with a hold live,
// is held to them. Mutation: gating on the room when idle refuses the first.
func TestIdleHeadAlwaysStarts(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	setLiveMem(t, 100) // below the reserve
	g := admitted(t, step(t, enqueue(t, e, need(4096))))
	if g.GrantMiB() != 4096 {
		t.Errorf("grant %d, want the 4096 asked", g.GrantMiB())
	}
	if r := step(t, enqueue(t, e, need(1))); isAdmitted(r) {
		t.Fatal("a request admitted beside a live hold with the room negative")
	}
}

// A container request's room leaves out the holds: of 8000 MiB, less the reserve, a hold's need of
// 40000 with a footprint of 100 leaves nothing to an ordinary request and 6976 MiB to it. Mutation:
// compute its room with the holds → not admitted, red.
func TestContainerIgnoresUnrealisedNeed(t *testing.T) {
	for _, req := range []spec.Request{container(), need(1)} {
		e := newEngine(t, newStateDir(t, ""), newFakeClock())
		setLiveMem(t, 8000)
		g := hold(t, e, need(40000))
		if err := g.UpdateHold(func(h *wire.Hold) { h.FootprintMiB = 100 }); err != nil {
			t.Fatal(err)
		}
		if got := isAdmitted(step(t, enqueue(t, e, req))); got != req.Container {
			t.Errorf("%+v: admitted %v, want %v", req, got, req.Container)
		}
	}
}

// Its room is still the real memory above the reserve: of 1000 MiB it has none, whatever the holds
// reserve. This passes without the container rule; it guards it. Mutation: reserve 0 in its room →
// admitted, red.
func TestContainerWaitsBelowTheReserve(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	setLiveMem(t, 1000)
	hold(t, e, need(1))
	if isAdmitted(step(t, enqueue(t, e, container()))) {
		t.Fatal("a container request admitted below the reserve")
	}
}

// The level gate holds it as it holds any request. This passes without the container rule; it guards
// it. Mutation: skip watchWait for it → admitted, red.
func TestContainerWaitsAtYellow(t *testing.T) {
	for _, level := range []watch.Level{watch.Yellow, watch.Red} {
		clk := newFakeClock()
		e := newEngine(t, capped(t, 8192), clk)
		hold(t, e, need(1))
		setWatch(t, e, clk, watch.Detector{Level: level}, time.Second)
		if isAdmitted(step(t, enqueue(t, e, container()))) {
			t.Errorf("a container request admitted at %v", level)
		}
	}
}

// A headroom that cannot be read gates nothing, without an error, and lets nobody pass a ticket
// ahead. Mutation: reading the headroom as 0 when it is not ok, and B waits.
func TestUnreadHeadroomGatesNothing(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	setLiveMem(t, 0)
	testHeadroom.ok = false
	admitted(t, step(t, enqueue(t, e, need(1024))))
	admitted(t, step(t, enqueue(t, e, need(2048))))
}

// A hold hands the room back by the footprint its run wrote: with 3584 MiB, A's 1 GiB with no
// footprint leaves 1536 MiB after the reserve, too little for B's 2 GiB. With A's footprint at
// 1 GiB nothing of it is still to grow, 2560 MiB are free and B is admitted. Mutation: ignoring
// the footprint keeps B out.
func TestRoomCountsFootprint(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	setLiveMem(t, 3584)
	a := admitted(t, step(t, enqueue(t, e, need(1024))))
	b := enqueue(t, e, need(2048))
	if isAdmitted(step(t, b)) {
		t.Fatal("B admitted beside A's whole need")
	}
	if err := a.UpdateHold(func(h *wire.Hold) { h.FootprintMiB = 1024 }); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second) // the head's next attempt
	admitted(t, step(t, b))
}

// A live hold whose body does not parse (its run rewrites it in place, so a read can be torn)
// counts at the cold start of the machine, 426 MiB of 1707, not at zero: 2048 MiB of headroom less
// the 1024 reserve and 426 leaves 598. Mutation: counting it at zero admits 600.
// Only a hold whose run still ticks is a job: one whose command exited while a descendant keeps
// the lock, one whose run stopped ticking and a torn body count neither in E nor as the oldest.
// Mutation: listing every live hold gives E 28 from the frozen hold and exempts it as the oldest.
func TestOnlyTickingHoldsAreJobs(t *testing.T) {
	now := int64(1e18)
	frozen := wire.Hold{Label: "f", ArrivalNS: 1, DUserNS: 8e8, DSysNS: 12e8, NormalShare: 0.04, Learned: true, Victim: true, ChildExited: true, SampledNS: now - 1e9}
	stale := wire.Hold{Label: "f", ArrivalNS: 2, DUserNS: 8e8, DSysNS: 12e8, NormalShare: 0.04, Learned: true, Victim: true, SampledNS: now - 7e9}
	oldest := wire.Hold{Label: "c", ArrivalNS: 3, DUserNS: 2e9, NormalShare: 0.04, Learned: true, Victim: true, SampledNS: now - 1e9}
	newest := wire.Hold{Label: "c", ArrivalNS: 4, DUserNS: 2e9, NormalShare: 0.04, Learned: true, Victim: true, SampledNS: now - 1e9,
		CPUNS: 1e9, TypicalCPUS: 10, TypicalS: 50, StallEWMA: 0.25}
	holds := []liveHold{{"frozen", frozen}, {"stale", stale}, {"oldest", oldest}, {"newest", newest},
		{"torn", wire.Hold{NeedMiB: 426}}}
	jobs := runningJobs(holds, now)
	if e := watch.Excess(jobs, 2e9); e != 0 {
		t.Errorf("E %v from holds that do not tick, want 0", e)
	}
	if got, _ := watch.StopList(jobs, false, 1, nil, 0); !slices.Equal(got, []string{"newest"}) {
		t.Errorf("stop list %v, want [newest]", got)
	}
	// The job carries what a red round estimates its remaining time from, as its run wrote it.
	// Mutation: leaving the four out of runningJobs gives the job no estimate.
	if j := jobs[len(jobs)-1]; j.CPUNS != 1e9 || j.TypicalCPUS != 10 || j.TypicalS != 50 || j.StallEWMA != 0.25 {
		t.Errorf("newest job %+v, want its hold's CPU time, typical CPU and run time, and stall share", j)
	}
}

// A job is learned only when its own run wrote so, and then with the share it wrote; two holds of
// one label can differ.
func TestRunningJobsAreLearnedByTheirRun(t *testing.T) {
	now := int64(1e18)
	holds := []liveHold{{"a", wire.Hold{Label: "x", SampledNS: now, Learned: true, NormalShare: 0.25}},
		{"b", wire.Hold{Label: "x", SampledNS: now}}}
	jobs := runningJobs(holds, now)
	if !jobs[0].Learned || jobs[0].NormalShare != 0.25 || jobs[1].Learned {
		t.Errorf("jobs = %+v, %+v; want a learned with 0.25, b learning", jobs[0], jobs[1])
	}
}

func TestTornLiveHoldCountsAtColdStart(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 2048)
	if err := os.MkdirAll(filepath.Join(dir, store.HoldsDir), 0o700); err != nil {
		t.Fatal(err)
	}
	torn := filepath.Join(dir, store.HoldsDir, wire.HoldName(1, 4242, 1, "torn"))
	if err := os.WriteFile(torn, []byte(`{"label":"torn","pid":42`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(torn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	over := enqueue(t, e, need(600))
	if r := step(t, over); isAdmitted(r) {
		t.Fatal("600 MiB admitted with the torn hold counted at zero")
	}
	if err := over.Withdraw(); err != nil {
		t.Fatal(err)
	}
	admitted(t, step(t, enqueue(t, e, need(598))))
}

// Two runs that read the run log at different moments sample one machine and agree on E: A1
// started before label x's first run completed, A3 after it. Each wrote its own view in its hold,
// so whoever samples, A1 is learning and A3 counts.
func TestSamplersWithDifferentLogViewsAgree(t *testing.T) {
	dir, clk := newStateDir(t, ""), newFakeClock()
	setLiveMem(t, 8192)
	early, late := newEngine(t, dir, clk), newEngine(t, dir, clk)
	for _, share := range []float64{0, 0.04} { // A1 learning, A3 learned
		g := hold(t, early, needLabel(100, "x"))
		if err := g.UpdateHold(func(h *wire.Hold) {
			h.DUserNS, h.DSysNS, h.SampledNS = 8e8, 12e8, clk.clock().Realtime()
			h.NormalShare, h.Learned = share, share > 0
		}); err != nil {
			t.Fatal(err)
		}
	}
	excess := func(e *Engine) float64 {
		s, err := e.sampler(config.Config{}, config.Machine{MemMiB: 1707})()
		if err != nil {
			t.Fatal(err)
		}
		return watch.Excess(s.Jobs, memsample.PeriodNS)
	}
	if a, b := excess(early), excess(late); a != b || a != 56 {
		t.Errorf("E is %v when the earlier run samples and %v when the later does; want 56 from A3 alone", a, b)
	}
}

// A stop round frees the physical shortage, the reserve less the headroom, not the reservations
// not yet used: with 1.5 GiB of headroom over a 1 GiB reserve nothing is short, and of A (6 GiB
// needed, 5 in use), B (3, 1), C (3, 0.8) and D (3, 0.5) the round lists D alone. Mutation: sizing
// the round by the room, -7.2 GiB here, lists D, C and B.
func TestStopRoundFreesTheShortageNotTheReservations(t *testing.T) {
	clk := newFakeClock()
	dir := newStateDir(t, "")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	big := func() (config.Machine, error) {
		return config.Machine{MemMiB: 65536, HeadroomMiB: 65536, HeadroomOK: true}, nil
	}
	e := New(st, filepath.Join(dir, store.ConfigFile), big, clk.clock())
	for i, h := range []struct{ need, footprint int64 }{{6144, 5120}, {3072, 1024}, {3072, 819}, {3072, 512}} {
		g := hold(t, e, need(h.need))
		if err := g.UpdateHold(func(b *wire.Hold) {
			b.ArrivalNS, b.FootprintMiB, b.Victim, b.Learned = int64(i+1), h.footprint, true, true
			b.SampledNS = clk.clock().Realtime()
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Headroom 1536 leaves nothing short; at 256 the machine is 768 MiB short, which D (512) does
	// not cover and D with C (819) does.
	for _, c := range []struct {
		headroom, short int64
		want            []int64 // arrivals
	}{{1536, 0, []int64{4}}, {256, 768, []int64{4, 3}}} {
		s, err := e.sampler(config.Config{ReserveMiB: 1024}, config.Machine{MemMiB: 65536, HeadroomMiB: c.headroom, HeadroomOK: true})()
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		stop, _ := watch.StopList(s.Jobs, false, s.ShortMiB, nil, 0)
		for _, hold := range stop {
			for _, j := range s.Jobs {
				if j.Hold == hold {
					got = append(got, j.ArrivalNS)
				}
			}
		}
		if len(s.Live) != 4 { // every live hold, for the kept records (watch.Prune). Mutation: leaving Live empty
			t.Errorf("sample lists %d live holds, want 4", len(s.Live))
		}
		if s.ShortMiB != c.short || !slices.Equal(got, c.want) {
			t.Errorf("headroom %d: shortage %d MiB, stopped arrivals %v (room %d); want %d, %v", c.headroom, s.ShortMiB, got, s.RoomMiB, c.short, c.want)
		}
	}
}
