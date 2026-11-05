//go:build unix

package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/memsample"
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
		s, err := e.sampler(config.Config{}, config.Machine{MemMiB: 1707})(watch.Sample{})
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
		s, err := e.sampler(config.Config{ReserveMiB: 1024}, config.Machine{MemMiB: 65536, HeadroomMiB: c.headroom, HeadroomOK: true})(watch.Sample{})
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

// macParts are macOS counts whose strict headroom is 1024 MiB and whose anonymous memory, 6024 MiB,
// makes the cheap one 1024 + 0.4 x (6024 - the live jobs' footprints).
var macParts = memsample.HeadroomParts{FreeMiB: 1024, FileBackedMiB: 1792, InternalMiB: 6024}

// macEngine is an engine on dir whose machine reads macParts, with their strict reading as the
// headroom, as memsample.Headroom gives it on macOS.
func macEngine(t *testing.T, dir string, clk *fakeClock) *Engine {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := func() (config.Machine, error) {
		p := macParts
		return config.Machine{MemMiB: 16384, HeadroomMiB: p.Strict(), HeadroomOK: true, HeadroomParts: &p}, nil
	}
	return New(st, filepath.Join(dir, store.ConfigFile), m, clk.clock())
}

// fixedHost makes every machine sample of the test read cur as the host, and returns the stored
// sample prev whose host lies one period before it, with compressed fewer bytes compressed and
// swapped out: a rate of compressed / 2 per second.
func fixedHost(t *testing.T, compressed uint64) (prev watch.Sample) {
	t.Helper()
	const s = int64(time.Second)
	cur := memsample.HostReading{CompressBytes: 1 << 40, WallNS: 1000 * s, AwakeNS: 900 * s, SwapFreeMiB: 8192, VMVolumeFreeMiB: 100000}
	saved := readHost
	t.Cleanup(func() { readHost = saved })
	readHost = func() (memsample.HostReading, error) { return cur, nil }
	prev.Host = cur
	prev.Host.CompressBytes -= compressed
	prev.Host.WallNS, prev.Host.AwakeNS = cur.WallNS-2*s, cur.AwakeNS-2*s
	return prev
}

// storeRate stores a green sample taken age ago, by the engine's clock, whose compression rate is
// rate (nil: none).
func storeRate(t *testing.T, e *Engine, clk *fakeClock, rate *float64, age time.Duration) {
	t.Helper()
	sec, err := e.st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if err := sec.SetWatch(watch.Detector{Level: watch.Green}, watch.Sample{CompressBytesS: rate}, clk.clock().Realtime()-int64(age)); err != nil {
		t.Fatal(err)
	}
}

// With a hold live, admission reads the macOS headroom in the mode the stored sample's
// compression rate chooses. A (1 GiB needed, all of it in use) leaves 5000 MiB of other anonymous
// memory, so the cheap room is 1024 + 2000 - 1024 = 2000 MiB and the strict one 0: 1500 MiB starts
// under a fresh slow rate, and waits under a fast one, a rate 7 s old, or none stored; 2200 MiB
// waits under any. Snapshot reports the same room. Mutations: inverting the switch admits under
// the fast rate and refuses under the slow; ignoring the sample's age admits under the stale one;
// forgetting the footprints in the sum (room 2409) admits the 2200; Snapshot reading the headroom
// without its mode reports 0 for the slow rate.
func TestAdmissionHeadroomFollowsTheStoredRate(t *testing.T) {
	slow, fast := 0.5*memsample.CompressHarmBytesS, 2.0*memsample.CompressHarmBytesS
	for _, c := range []struct {
		name  string
		rate  *float64
		age   time.Duration
		need  int64
		admit bool
		free  int64
	}{
		{"slow", &slow, time.Second, 1500, true, 2000},
		{"fast", &fast, time.Second, 1500, false, 0},
		{"stale", &slow, 7 * time.Second, 1500, false, 0},
		{"no rate", nil, time.Second, 1500, false, 0},
		{"slow, over the cheap room", &slow, time.Second, 2200, false, 2000},
	} {
		clk := newFakeClock()
		e := macEngine(t, newStateDir(t, ""), clk)
		if err := hold(t, e, need(1024)).UpdateHold(func(h *wire.Hold) { h.FootprintMiB = 1024 }); err != nil {
			t.Fatal(err)
		}
		storeRate(t, e, clk, c.rate, c.age)
		r, err := e.Snapshot()
		if err != nil || r.Mem.FreeMiB != c.free {
			t.Errorf("%s: Snapshot free %d MiB, %v; want %d", c.name, r.Mem.FreeMiB, err, c.free)
		}
		if got := isAdmitted(step(t, enqueue(t, e, need(c.need)))); got != c.admit {
			t.Errorf("%s: %d MiB admitted %v, want %v", c.name, c.need, got, c.admit)
		}
	}
}

// Elsewhere than macOS, and under the test hook, the headroom is the kernel's reading as it is,
// whatever the rate and the footprints. Mutation: subtracting the footprints from it, or reading
// it through the macOS modes, fails it.
func TestHeadroomUntouchedWithoutParts(t *testing.T) {
	holds := []liveHold{{name: "a", body: wire.Hold{NeedMiB: 2048, FootprintMiB: 1500}}}
	fast, slow := 2.0*memsample.CompressHarmBytesS, 0.0
	for i, rate := range []*float64{nil, &fast, &slow} {
		if mib, mode := headroom(config.Machine{HeadroomMiB: 777, HeadroomOK: true}, holds, rate); mib != 777 || mode != "" {
			t.Errorf("%s rate: headroom %d, %q; want 777, none", []string{"no", "fast", "slow"}[i], mib, mode)
		}
	}
}

// A machine sample carries the compression rate since the stored sample, the headroom its room
// came from in the mode that rate chooses, its parts and mode, and the cheap reading its shortage
// came from, whatever the mode. With no stored sample the rate is unknown and, a hold live, the
// room's headroom strict. Without parts (Linux) no rate is kept and both are the kernel's; an
// unknown headroom carries none of it. The counters are fixed, not live. Mutations: not storing
// the rate fails "slow"; the shortage from the room's headroom fails "fast" and "unknown"; the
// room from the strict reading in every mode fails "slow"; not copying the headroom into the
// sample fails every row.
func TestSampleCarriesTheHeadroom(t *testing.T) {
	// A's footprint of 1024 leaves 5000 MiB of other anonymous memory: cheap 1024 + 2000 = 3024.
	const strict, cheap, reserve = 1024, 3024, 4096
	for _, c := range []struct {
		name       string
		compressed uint64 // over the 2 s period; 0 with no stored sample
		stored     bool
		rate       float64
		mib        int64
		mode       string
	}{
		{"slow", 1 << 30, true, 0.5 * memsample.CompressHarmBytesS, cheap, memsample.HeadroomCheap},
		{"fast", 4 << 30, true, 2 * memsample.CompressHarmBytesS, strict, memsample.HeadroomStrict},
		{"unknown", 0, false, 0, strict, memsample.HeadroomStrict},
	} {
		dir, clk := newStateDir(t, ""), newFakeClock()
		e := macEngine(t, dir, clk)
		if err := hold(t, e, need(1024)).UpdateHold(func(h *wire.Hold) { h.FootprintMiB = 1024 }); err != nil {
			t.Fatal(err)
		}
		prev := fixedHost(t, c.compressed)
		if !c.stored {
			prev = watch.Sample{}
		}
		m, _ := e.machine()
		s, err := e.sampler(config.Config{ReserveMiB: reserve}, m)(prev)
		if err != nil || s.HeadroomMiB == nil || s.HeadroomCheapMiB == nil || s.HeadroomParts == nil || *s.HeadroomParts != macParts {
			t.Fatalf("%s: %+v, %v; want a headroom, its cheap reading and parts", c.name, s, err)
		}
		if c.stored != (s.CompressBytesS != nil) || c.stored && *s.CompressBytesS != c.rate {
			t.Errorf("%s: rate %v; want %v (stored %v)", c.name, s.CompressBytesS, c.rate, c.stored)
		}
		if *s.HeadroomMiB != c.mib || s.HeadroomMode != c.mode || s.RoomMiB != c.mib-reserve ||
			*s.HeadroomCheapMiB != cheap || s.ShortMiB != reserve-cheap {
			t.Errorf("%s: headroom %d %s, room %d, cheap %d, short %d; want %d %s, %d, %d, %d", c.name, *s.HeadroomMiB,
				s.HeadroomMode, s.RoomMiB, *s.HeadroomCheapMiB, s.ShortMiB, c.mib, c.mode, c.mib-reserve, cheap, reserve-cheap)
		}
	}
	dir, clk := newStateDir(t, ""), newFakeClock()
	e := macEngine(t, dir, clk)
	prev := fixedHost(t, 1<<30)
	cfg := config.Config{ReserveMiB: reserve}
	if s, err := e.sampler(cfg, config.Machine{MemMiB: 8192, HeadroomMiB: 3000, HeadroomOK: true})(prev); err != nil ||
		s.CompressBytesS != nil || s.HeadroomMiB == nil || *s.HeadroomMiB != 3000 || s.HeadroomMode != "" ||
		s.HeadroomParts != nil || s.HeadroomCheapMiB != nil || s.ShortMiB != reserve-3000 {
		t.Errorf("no parts: %+v, %v; want no rate, the kernel's 3000 for room and shortage, no mode", s, err)
	}
	p := macParts
	if s, err := e.sampler(cfg, config.Machine{MemMiB: 8192, HeadroomParts: &p})(prev); err != nil ||
		s.HeadroomMiB != nil || s.HeadroomParts != nil || s.CompressBytesS != nil || s.HeadroomMode != "" || s.HeadroomCheapMiB != nil {
		t.Errorf("unknown headroom: %+v, %v; want no headroom, parts, rate or mode", s, err)
	}
}

// A stop round's shortage reads the cheap headroom whatever the compression rate, so the same
// pressure lists the same victims. Of A (the oldest, 2000 MiB in use) and B, C and D (400 MiB each),
// the other anonymous memory is 4600 - 3200 = 1400 MiB: cheap 100 + 560 = 660, a shortage of
// 1024 - 660 = 364 that D alone covers; the strict reading, 100, would be 924 short and list D, C
// and B. Mutation: sizing the shortage by the room's headroom lists three under the fast rate and
// with no rate.
func TestStopRoundShortageReadsCheap(t *testing.T) {
	parts := memsample.HeadroomParts{FreeMiB: 100, FileBackedMiB: 1792, InternalMiB: 4600}
	for _, c := range []struct {
		name       string
		compressed uint64
		stored     bool
		mode       string
	}{
		{"fast", 4 << 30, true, memsample.HeadroomStrict},
		{"slow", 1 << 30, true, memsample.HeadroomCheap},
		{"unknown", 0, false, memsample.HeadroomStrict},
	} {
		clk := newFakeClock()
		dir := newStateDir(t, "")
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		big := func() (config.Machine, error) { // admits the four; the sample reads parts
			return config.Machine{MemMiB: 65536, HeadroomMiB: 65536, HeadroomOK: true}, nil
		}
		e := New(st, filepath.Join(dir, store.ConfigFile), big, clk.clock())
		for i, h := range []struct{ need, footprint int64 }{{2048, 2000}, {512, 400}, {512, 400}, {512, 400}} {
			if err := hold(t, e, need(h.need)).UpdateHold(func(b *wire.Hold) {
				b.ArrivalNS, b.FootprintMiB, b.Victim, b.Learned = int64(i+1), h.footprint, true, true
				b.SampledNS = clk.clock().Realtime()
			}); err != nil {
				t.Fatal(err)
			}
		}
		prev := fixedHost(t, c.compressed)
		if !c.stored {
			prev = watch.Sample{}
		}
		p := parts
		s, err := e.sampler(config.Config{ReserveMiB: 1024}, config.Machine{MemMiB: 16384, HeadroomMiB: p.Strict(), HeadroomOK: true, HeadroomParts: &p})(prev)
		if err != nil {
			t.Fatal(err)
		}
		stop, _ := watch.StopList(s.Jobs, false, s.ShortMiB, nil, 0)
		var got []int64
		for _, hold := range stop {
			for _, j := range s.Jobs {
				if j.Hold == hold {
					got = append(got, j.ArrivalNS)
				}
			}
		}
		if s.HeadroomMode != c.mode || s.ShortMiB != 364 || !slices.Equal(got, []int64{4}) {
			t.Errorf("%s: mode %s, shortage %d, stopped arrivals %v; want %s, 364, [4]", c.name, s.HeadroomMode, s.ShortMiB, got, c.mode)
		}
	}
}

// Every live hold's footprint is left out of the anonymous memory, a run outside the budget's
// (need 0) too, in admission's headroom and the shortage's alike: 1024 + 0.4 x (6024 - 1500) =
// 2833. Mutation: summing only the holds that reserve memory reads 3033.
func TestFootprintsOfEveryLiveHold(t *testing.T) {
	p := macParts
	m := config.Machine{HeadroomMiB: p.Strict(), HeadroomOK: true, HeadroomParts: &p}
	holds := []liveHold{{name: "a", body: wire.Hold{NeedMiB: 1024, FootprintMiB: 1000}},
		{name: "b", body: wire.Hold{NeedMiB: 0, FootprintMiB: 500}}}
	slow := 0.0
	if mib, mode := headroom(m, holds, &slow); mib != 2833 || mode != memsample.HeadroomCheap {
		t.Errorf("headroom %d, %s; want 2833, cheap", mib, mode)
	}
	if mib := shortHeadroom(m, holds); mib != 2833 {
		t.Errorf("shortage headroom %d; want 2833", mib)
	}
}
