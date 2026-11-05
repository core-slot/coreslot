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

// Red needs red_samples hot samples in a row from an empty bucket: 10 by default, so the ninth is
// yellow and the tenth red. Mutation: a default of 5 turns the ninth red.
func TestRedAfterTenHotSamples(t *testing.T) {
	for _, c := range []struct {
		hot  int
		want Level
	}{{9, Yellow}, {10, Red}} {
		cfg := Defaults()
		f := newFeed(cfg)
		var l Level
		for range c.hot {
			l = f.step(cfg, true, job(0.60, 0.04))
		}
		if l != c.want {
			t.Errorf("%d hot samples: %v (bucket %v), want %v", c.hot, l, f.d.Bucket, c.want)
		}
	}
}

// Yellow clears after 15 samples in a row below yellow_points. After two hot samples the first calm
// one still sees two hot ones among the last three and keeps the latch, so the count starts at the
// second; a sample at or over yellow_points with the gate shut is not hot but restarts it.
// Mutation: clearing after 5 turns the yellow rows green.
func TestYellowClearsAfterFifteenBelow(t *testing.T) {
	calm, warm := job(0.04, 0.04), job(0.50, 0.25) // E 0 and E 25
	for _, c := range []struct {
		name string
		warm bool // the fourth calm sample is warm, with the gate shut
		calm int  // calm samples after the hot ones, the warm one included
		want Level
	}{{"14 below", false, 15, Yellow}, {"15 below", false, 16, Green},
		{"14 below after a warm one", true, 18, Yellow}, {"15 below after a warm one", true, 19, Green}} {
		t.Run(c.name, func(t *testing.T) {
			cfg := Defaults()
			f := newFeed(cfg)
			f.step(cfg, true, job(0.60, 0.04))
			f.step(cfg, true, job(0.60, 0.04))
			var l Level
			for i := range c.calm {
				if c.warm && i == 3 {
					l = f.step(cfg, false, warm)
				} else {
					l = f.step(cfg, false, calm)
				}
			}
			if l != c.want {
				t.Errorf("after %d calm samples: %v (below %d), want %v", c.calm, l, f.d.Below, c.want)
			}
		})
	}
}

// The yellow latch keeps only fresh evidence, as the bucket does: a real reading more than 20 s
// after the last real one, or before it, clears it, so the calm sample after it is green; within
// 20 s, across a short sleep too, the latch holds and the calm sample is yellow. Mutations: never
// clearing the latch turns the stale rows yellow; dropping the clock-back term turns that row
// yellow; clearing it at 20 s turns that row green.
func TestYellowLatchKeepsOnlyFreshEvidence(t *testing.T) {
	for _, c := range []struct {
		name            string
		wallNS, awakeNS int64 // the gap reading's clocks, from the last real one's
		want            Level
	}{{"10 s period", 10e9, 10e9, Yellow}, {"20 s period", 20e9, 20e9, Yellow},
		{"sleep 15 s", 17e9, 2e9, Yellow}, {"21 s period", 21e9, 21e9, Green},
		{"sleep 600 s", 600e9, 2e9, Green}, {"clock went back 10 s", -10e9, -10e9, Green}} {
		t.Run(c.name, func(t *testing.T) {
			cfg := Defaults()
			f := newFeed(cfg)
			for range 3 {
				f.step(cfg, true, job(0.60, 0.04))
			}
			if f.d.Level != Yellow || f.d.Bucket >= cfg.RedSamples {
				t.Fatalf("the fixture: %v, bucket %v; want yellow under red", f.d.Level, f.d.Bucket)
			}
			gap := f.prev
			gap.Host.WallNS += c.wallNS
			gap.Host.AwakeNS += c.awakeNS
			gap.Jobs = nil
			if l := f.d.Advance(f.prev, gap, cfg); l != Unknown {
				t.Fatalf("across the gap: %v, want unknown", l)
			}
			f.prev = gap
			if l := f.step(cfg, false, job(0.04, 0.04)); l != c.want {
				t.Errorf("one calm sample after the gap: %v (yellow latch %v), want %v", l, f.d.Yellow, c.want)
			}
		})
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

// A sleep (the sleep-inclusive clock moved, the awake one 2 s), a long period or a reboot (the clock
// or a counter went back) between two samples is a gap: unknown, whatever the deltas say. The red
// bucket and the yellow latch keep only fresh evidence: a reading more than 20 s after the last real
// one, or before it, empties both, so one hot sample at a bucket of 9.5 is green; within 20 s it is
// red, across a short sleep or a reboot alike. Mutations: never emptying the bucket turns the stale
// rows red; emptying it at 20 s turns that row green; dropping the clock-back term turns that row
// red; emptying it on any sleep turns the 15 s sleep green.
func TestGapIsUnknown(t *testing.T) {
	for _, c := range []struct {
		name            string
		wallNS, awakeNS int64 // cur's clocks, from prev's
		back            bool  // cur's counters restarted from 0
		want            Level
	}{{"sleep 600 s", 600e9, 2e9, false, Green}, {"an hour awake", 3600e9, 3600e9, false, Green},
		{"21 s period", 21e9, 21e9, false, Green}, {"clock went back", -1e12, -1e12, false, Green},
		{"10 s period", 10e9, 10e9, false, Red}, {"20 s period", 20e9, 20e9, false, Red},
		{"sleep 15 s", 17e9, 2e9, false, Red}, {"reboot, uptime 2 s later", 2e9, 2e9, true, Red}} {
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
			if f.pages += 1e6; c.back {
				f.pages = 0
			}
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
}

// An empty sample (a reading that could not be read) keeps the bucket and leaves the window, whose
// newest point stays the last real reading: under sustained danger, a real reading 4 s after it
// keeps red going at once; one 600 s after it (a sleep the empty sample hid) empties the bucket and
// the yellow latch.
// Mutations: emptying the bucket on the pair after an empty sample delays red; letting the empty
// sample clear the window does too; never emptying the bucket turns the hidden sleep red.
func TestEmptySampleKeepsTheBucket(t *testing.T) {
	for _, c := range []struct {
		name          string
		wallNS, awake int64 // the next real reading's clocks, from the last real one's
		want          Level
	}{{"4 s later", 4e9, 4e9, Red}, {"after a 600 s sleep", 600e9, 4e9, Green}} {
		t.Run(c.name, func(t *testing.T) {
			cfg := Defaults()
			f := newFeed(cfg)
			for range 10 {
				f.step(cfg, true, job(0.60, 0.04))
			}
			if l := f.d.Advance(f.prev, Sample{}, cfg); l != Unknown || f.d.Bucket != cfg.RedSamples {
				t.Errorf("an empty sample: %v, bucket %v; want unknown, bucket %v", l, f.d.Bucket, cfg.RedSamples)
			}
			cur := f.prev
			cur.Host.WallNS += c.wallNS
			cur.Host.AwakeNS += c.awake
			if l := f.d.Advance(Sample{}, cur, cfg); l != Unknown {
				t.Errorf("after the empty sample: %v, want unknown", l)
			}
			f.prev = cur
			if l := f.step(cfg, true, job(0.60, 0.04)); l != c.want {
				t.Errorf("one hot sample %s: %v (bucket %v), want %v", c.name, l, f.d.Bucket, c.want)
			}
		})
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
	for range clearSamples { // the first still has two hot samples among the last three
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
	if got := stopList(jobs(), false, 1024); !slices.Equal(got, []string{"7", "6", "5"}) {
		t.Errorf("stop list %v, want [7 6 5]", got)
	}
	if got := stopList(jobs(), false, 1<<20); !slices.Equal(got, []string{"7", "6", "5"}) {
		t.Errorf("stop list for a deficit nobody covers %v, want [7 6 5]", got)
	}
}

// A round stops at least one job, even with no deficit.
func TestStopListAtLeastOne(t *testing.T) {
	if got := stopList(jobs(), false, 0); !slices.Equal(got, []string{"7"}) {
		t.Errorf("stop list %v, want [7]", got)
	}
}

// With only the oldest job and jobs that may not be stopped left, the round lists nothing.
func TestStopListOnlyOldestLeft(t *testing.T) {
	js := jobs()
	for i := range js[1:] {
		js[i+1].Victim = false
	}
	if got := stopList(js, false, 1024); len(got) != 0 {
		t.Errorf("stop list %v, want none", got)
	}
	if got := stopList(jobs()[:1], false, 1024); len(got) != 0 {
		t.Errorf("stop list of the oldest alone %v, want none", got)
	}
}

// A red round never lists a cold job, even as the newest; the learned victims behind it are.
func TestStopListSkipsColdJobs(t *testing.T) {
	js := jobs()
	js[3].Learned = false
	if got := stopList(js, false, 1<<20); !slices.Equal(got, []string{"6", "5"}) {
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
		if got := stopList(js, c.floor, c.short); !slices.Equal(got, c.want) {
			t.Errorf("floor %v, shortage %d: stop list %v, want %v", c.floor, c.short, got, c.want)
		}
	}
}

// stopList is StopList with no job kept before.
func stopList(jobs []Job, floor bool, shortMiB int64) []string {
	stop, _ := StopList(jobs, floor, shortMiB, nil, 0)
	return stop
}
