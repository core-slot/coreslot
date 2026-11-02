package watch

import (
	"math"
	"slices"
	"testing"

	"github.com/core-slot/coreslot/internal/memsample"
)

// feed drives a detector with one sample every 2 s, as the watchdog takes them.
type feed struct {
	d                                        Detector
	prev                                     Sample
	pages                                    uint64
	swapUsed, swapFree, vmFree, memAvailable int64
}

// newFeed starts a feed on a machine with room in swap; its first sample has no predecessor.
func newFeed(c Config) *feed {
	f := &feed{swapFree: 8192, vmFree: 100000}
	f.prev.Host.WallNS = 1e12
	f.prev.Host.AwakeNS = 1e12
	return f
}

// step takes the next sample with the paging gate open or closed and returns the level.
func (f *feed) step(c Config, gate bool, jobs ...Job) Level {
	ns := f.prev.Host.WallNS + 2e9
	if gate {
		f.pages += 5000 // 2500/s, above the gate's 1000/s
	}
	cur := Sample{Host: memsample.HostReading{Decomp: f.pages, SwapUsedMiB: f.swapUsed, SwapFreeMiB: f.swapFree,
		VMVolumeFreeMiB: f.vmFree, MemAvailableMiB: f.memAvailable, WallNS: ns, AwakeNS: f.prev.Host.AwakeNS + 2e9}, Jobs: jobs}
	l := f.d.Advance(f.prev, cur, c)
	f.prev = cur
	return l
}

// job is one hold that spent 2 s with share of it in the system, its normal share normal.
func job(share, normal float64) Job {
	sys := int64(share * 2e9)
	return Job{Hold: "h", DUserNS: 2e9 - sys, DSysNS: sys, NormalShare: normal, Learned: true}
}

// The Air's ramp, at the cliff: 0.8 s user, 0.6 s system and 0.6 s waiting on pages in a period
// is S 60 % against a normal 4 %: 56 points.
func TestExcessFromAirRamp(t *testing.T) {
	j := Job{DUserNS: 8e8, DSysNS: 6e8, DPageWaitNS: 6e8, NormalShare: 0.04, Learned: true}
	if e := Excess([]Job{j}, 2e9); math.Abs(e-56) > 1e-9 {
		t.Errorf("excess %v, want 56", e)
	}
	// Weighted by time: a calm job with three times the time pulls it to a quarter.
	calm := Job{DUserNS: 6e9, NormalShare: 0.04, Learned: true}
	if e := Excess([]Job{j, calm}, 2e9); math.Abs(e-14) > 1e-9 {
		t.Errorf("excess with a calm job %v, want 14", e)
	}
}

// go test -race spends about a quarter in the system on a calm machine: with the gate open and
// S 27 % against its normal 25 %, the machine stays green.
func TestHighNormalShareNotYellow(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	for i := range 30 {
		if l := f.step(c, true, job(0.27, 0.25)); l != Green {
			t.Fatalf("sample %d: %v, want green", i, l)
		}
	}
}

// Two idle polling jobs spend 80 % of their few milliseconds in the system, and the gate is open at
// idle on a Mac: with under a tenth of the period between them, they carry no excess and the
// machine stays green.
func TestIdleJobsCarryNoExcess(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	idle := Job{DUserNS: 5e6, DSysNS: 20e6, NormalShare: 0.04, Learned: true}
	for i := range 30 {
		if l := f.step(c, true, idle, idle); l != Green {
			t.Fatalf("sample %d: %v, want green", i, l)
		}
	}
	if f.d.Excess != 0 {
		t.Errorf("excess %v with no job at work, want 0", f.d.Excess)
	}
}

// 25 points of excess are yellow only with the paging gate open: system time without paging is
// not memory.
func TestYellowNeedsGate(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	for i := range 10 {
		if l := f.step(c, false, job(0.50, 0.25)); l != Green {
			t.Fatalf("gate closed, sample %d: %v, want green", i, l)
		}
	}
	f.step(c, true, job(0.50, 0.25))
	if l := f.step(c, true, job(0.50, 0.25)); l != Yellow {
		t.Errorf("gate open in 2 of 3: %v, want yellow", l)
	}
}

// Red samples alternating with clean ones fill the bucket by half a sample per pair: it reaches
// red, which a timer reset by every clean sample would never do.
func TestRedLeakyBucketSurvivesFlicker(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	red := false
	for range 25 {
		red = red || f.step(c, true, job(0.60, 0.04)) == Red
		red = red || f.step(c, false, job(0.04, 0.04)) == Red
	}
	if !red {
		t.Errorf("never red; bucket %v", f.d.Bucket)
	}
}

// The bucket saturates at red_samples: after 15 hot samples the first calm one leaves red, so a
// calm machine gets no second round. Mutation: an uncapped bucket stays red for 10 calm samples.
func TestRedEndsOnTheFirstCalmSample(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	for range 15 {
		f.step(c, true, job(0.60, 0.04))
	}
	if f.d.Level != Red {
		t.Fatalf("after 15 hot samples: %v, want red", f.d.Level)
	}
	for i := range 20 {
		if l := f.step(c, false, job(0.04, 0.04)); l >= Red {
			t.Fatalf("calm sample %d: %v", i+1, l)
		}
	}
}

// One sample at 43 points, then calm: the bucket holds 1 and drains; never red.
func TestSpikeDoesNotRed(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	f.step(c, true, job(0.68, 0.25))
	if f.d.Bucket != 1 {
		t.Fatalf("bucket after the spike %v, want 1", f.d.Bucket)
	}
	for i := range 30 {
		if l := f.step(c, false, job(0.25, 0.25)); l == Red {
			t.Fatalf("sample %d after the spike: red", i)
		}
	}
	if f.d.Bucket != 0 {
		t.Errorf("bucket %v after calm, want 0", f.d.Bucket)
	}
}

// Floor is an edge: 1 GiB of swap room with swap flat is a calm machine with little room; 300 MiB
// more swap within 20 s at that room is floor.
func TestFloorIsAnEdge(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	f.swapFree, f.vmFree, f.swapUsed = 1024, 0, 4096
	for i := range 15 {
		if l := f.step(c, false); l == Floor {
			t.Fatalf("swap flat, sample %d: floor", i)
		}
	}
	var l Level
	for range 10 {
		f.swapUsed += 30
		l = f.step(c, false)
	}
	if l != Floor {
		t.Errorf("swap +300 MiB in 20 s at 1 GiB of room: %v, want floor", l)
	}
}

// On Linux the swap room counts MemAvailable: with 1 GiB of swap left but 8 GiB available, 300 MiB
// more swap within 20 s is ordinary paging, not floor. Mutation: swap room from SwapFree alone.
func TestFloorCountsMemAvailable(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	f.swapFree, f.vmFree, f.swapUsed, f.memAvailable = 1024, 0, 1024, 8192
	for range 10 {
		f.swapUsed += 30
		if l := f.step(c, false); l == Floor {
			t.Fatalf("swap +%d MiB with 8 GiB available: floor", f.swapUsed-1024)
		}
	}
}

// A sleep between two samples (the sleep-inclusive clock moved 600 s, the awake one 2 s) or a
// reboot (the clock went back) is a gap: unknown, whatever the deltas say, and red needs its samples
// after it, so one hot sample at a bucket of 9.5 is not red; a 10 s period without a sleep keeps the
// bucket, and that sample is, as does an empty sample (a reading that could not be read).
// Mutations: keeping the bucket across a sleep turns it red; emptying it on any gap empties it on
// the empty sample.
func TestGapIsUnknown(t *testing.T) {
	for _, c := range []struct {
		name            string
		wallNS, awakeNS int64 // cur's clocks, from prev's
		want            Level
	}{{"sleep", 600e9, 2e9, Yellow}, {"reboot", -1e12, -1e12, Yellow}, {"10 s period", 10e9, 10e9, Red}} {
		t.Run(c.name, func(t *testing.T) {
			cfg := Defaults()
			f := newFeed(cfg)
			for range 10 {
				f.step(cfg, true, job(0.60, 0.04))
			}
			f.step(cfg, false, job(0.04, 0.04))
			cur := f.prev
			cur.Host.WallNS += c.wallNS
			cur.Host.AwakeNS += c.awakeNS
			f.pages += 1e6
			cur.Host.Decomp = f.pages
			cur.Jobs = []Job{job(0.60, 0.04)}
			if !memsample.Gap(f.prev.Host, cur.Host) {
				t.Fatal("the fixture is not a gap")
			}
			if l := f.d.Advance(f.prev, cur, cfg); l != Unknown {
				t.Errorf("across the gap: %v, want unknown", l)
			}
			f.prev = cur
			if l := f.step(cfg, true, job(0.60, 0.04)); l != c.want {
				t.Errorf("one hot sample after the gap: %v (bucket %v), want %v", l, f.d.Bucket, c.want)
			}
		})
	}
	cfg := Defaults()
	f := newFeed(cfg)
	for range 10 {
		f.step(cfg, true, job(0.60, 0.04))
	}
	if l := f.d.Advance(f.prev, Sample{}, cfg); l != Unknown || f.d.Bucket != cfg.RedSamples {
		t.Errorf("an empty sample: %v, bucket %v; want unknown, bucket %v", l, f.d.Bucket, cfg.RedSamples)
	}
}

// The floor needs no rate: a 10 s period (a gap, so no rates) that ends under the floor with 300 MiB
// more swap used than the window's low is floor; the same pair above the floor stays unknown.
// Mutation: returning unknown before the floor rule makes both unknown.
func TestFloorAcrossAGap(t *testing.T) {
	for _, c := range []struct {
		name     string
		swapFree int64
		want     Level
	}{{"under the floor", 1024, Floor}, {"above the floor", 8192, Unknown}} {
		t.Run(c.name, func(t *testing.T) {
			cfg := Defaults()
			f := newFeed(cfg)
			f.swapFree, f.vmFree, f.swapUsed = c.swapFree, 0, 4096
			for range 3 {
				f.step(cfg, false)
			}
			cur := f.prev
			cur.Host.WallNS += 10e9
			cur.Host.AwakeNS += 10e9
			cur.Host.SwapUsedMiB += 300
			if l := f.d.Advance(f.prev, cur, cfg); l != c.want || f.d.Level != c.want || c.want == Floor && f.d.Known != Floor {
				t.Errorf("a 10 s period with +300 MiB of swap: %v (known %v), want %v", l, f.d.Known, c.want)
			}
		})
	}
}

// A clear that comes through a gap is still a clear: yellow, a 10 s period (unknown), then the
// calm sample that ends the latch turns green and records yellow_cleared_ns. Mutation: comparing
// with the level just before, which is unknown, records nothing.
func TestClearThroughUnknownIsRecorded(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	for range 3 {
		f.step(c, true, job(0.60, 0.04))
	}
	for range 15 { // the first still has two hot samples among the last three
		f.step(c, false, job(0.04, 0.04))
	}
	gap := f.prev
	gap.Host.WallNS += 10e9
	gap.Host.AwakeNS += 10e9
	if l := f.d.Advance(f.prev, gap, c); l != Unknown {
		t.Fatalf("a 10 s period: %v, want unknown", l)
	}
	f.prev = gap
	if l := f.step(c, false, job(0.04, 0.04)); l != Green || f.d.YellowClearedNS != f.prev.Host.WallNS {
		t.Errorf("after the gap: %v, yellow_cleared_ns %d; want green at %d", l, f.d.YellowClearedNS, f.prev.Host.WallNS)
	}
}

// A platform without PSI or page wait still has levels: they are optional readings.
func TestMissingOptionalReadingIsNotUnknown(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	var levels []Level
	for range 3 {
		levels = append(levels, f.step(c, true, job(0.60, 0.04)))
	}
	if f.prev.Host.HasPSI || slices.Contains(levels, Unknown) || levels[2] != Yellow {
		t.Errorf("levels %v without PSI or page wait, want yellow by the third", levels)
	}
}

// A job whose label has no learned normal S is no part of E, however much of its time it spends
// in the system: 0.82 with the gate open stays green and carries no excess.
func TestColdJobCarriesNoExcess(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	cold := job(0.82, 0)
	cold.Learned = false
	for i := range 30 {
		if l := f.step(c, true, cold); l != Green || f.d.Excess != 0 {
			t.Fatalf("sample %d: %v, E %v; want green, E 0", i, l, f.d.Excess)
		}
	}
}

// E comes from the learned jobs alone: a cold job with three times the time does not dilute it.
func TestExcessIgnoresColdJobs(t *testing.T) {
	learned := Job{DUserNS: 8e8, DSysNS: 6e8, DPageWaitNS: 6e8, NormalShare: 0.04, Learned: true}
	cold := Job{DUserNS: 6e9, DSysNS: 6e9}
	if e := Excess([]Job{learned, cold}, 2e9); math.Abs(e-56) > 1e-9 {
		t.Errorf("excess %v with a cold job, want 56", e)
	}
}

func jobs() []Job {
	return []Job{
		{Hold: "1", ArrivalNS: 1, FootprintMiB: 8192, Victim: true, Learned: true},
		{Hold: "5", ArrivalNS: 5, FootprintMiB: 307, Victim: true, Learned: true},
		{Hold: "6", ArrivalNS: 6, FootprintMiB: 410, Victim: true, Learned: true},
		{Hold: "7", ArrivalNS: 7, FootprintMiB: 512, Victim: true, Learned: true},
	}
}

// Victims go newest first until their footprints cover the deficit; the oldest never goes.
func TestStopListNewestFirstCumulative(t *testing.T) {
	if got := StopList(jobs(), false, 1024); !slices.Equal(got, []string{"7", "6", "5"}) {
		t.Errorf("stop list %v, want [7 6 5]", got)
	}
	if got := StopList(jobs(), false, 1<<20); !slices.Equal(got, []string{"7", "6", "5"}) {
		t.Errorf("stop list for a deficit nobody covers %v, want [7 6 5]", got)
	}
}

// A round stops at least one job, even with no deficit.
func TestStopListAtLeastOne(t *testing.T) {
	if got := StopList(jobs(), false, 0); !slices.Equal(got, []string{"7"}) {
		t.Errorf("stop list %v, want [7]", got)
	}
}

// With only the oldest job and jobs that may not be stopped left, the round lists nothing.
func TestStopListOnlyOldestLeft(t *testing.T) {
	js := jobs()
	for i := range js[1:] {
		js[i+1].Victim = false
	}
	if got := StopList(js, false, 1024); len(got) != 0 {
		t.Errorf("stop list %v, want none", got)
	}
	if got := StopList(jobs()[:1], false, 1024); len(got) != 0 {
		t.Errorf("stop list of the oldest alone %v, want none", got)
	}
}

// A red round never lists a cold job, even as the newest; the learned victims behind it are.
func TestStopListSkipsColdJobs(t *testing.T) {
	js := jobs()
	js[3].Learned = false
	if got := StopList(js, false, 1<<20); !slices.Equal(got, []string{"6", "5"}) {
		t.Errorf("stop list %v, want [6 5]", got)
	}
}

// A is the oldest and learned, B learning and large, D learning and small, C learned, small and newest.
// A red round lists C alone; a floor round lists the learned C, then the learning ones newest
// first until the footprints cover the shortage, and never A. Mutation: ignoring the floor flag
// lists C alone to the end.
func TestStopListFloorTakesLearningJobs(t *testing.T) {
	js := []Job{
		{Hold: "A", ArrivalNS: 1, FootprintMiB: 2048, Victim: true, Learned: true},
		{Hold: "B", ArrivalNS: 2, FootprintMiB: 8192, Victim: true},
		{Hold: "C", ArrivalNS: 4, FootprintMiB: 300, Victim: true, Learned: true},
		{Hold: "D", ArrivalNS: 3, FootprintMiB: 100, Victim: true},
	}
	for _, c := range []struct {
		floor bool
		short int64
		want  []string
	}{{false, 1 << 20, []string{"C"}}, {true, 200, []string{"C"}}, {true, 350, []string{"C", "D"}},
		{true, 1 << 20, []string{"C", "D", "B"}}} {
		if got := StopList(js, c.floor, c.short); !slices.Equal(got, c.want) {
			t.Errorf("floor %v, shortage %d: stop list %v, want %v", c.floor, c.short, got, c.want)
		}
	}
}
