package watch

import (
	"math"
	"slices"
	"testing"

	"github.com/core-slot/coreslot/internal/memsample"
)

// feed drives a detector with one sample every 2 s, as the watchdog takes them.
type feed struct {
	d                          Detector
	prev                       Sample
	pages                      uint64
	swapUsed, swapFree, vmFree int64
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
	cur := Sample{Host: memsample.HostReading{Decomp: f.pages, SwapUsedMiB: f.swapUsed,
		SwapFreeMiB: f.swapFree, VMVolumeFreeMiB: f.vmFree, WallNS: ns, AwakeNS: ns}, Jobs: jobs}
	l := f.d.Advance(f.prev, cur, c)
	f.prev = cur
	return l
}

// job is one hold that spent 2 s with share of it in the system, its normal share normal.
func job(share, normal float64) Job {
	sys := int64(share * 2e9)
	return Job{Hold: "h", DUserNS: 2e9 - sys, DSysNS: sys, NormalShare: normal}
}

// The Air's ramp, at the cliff: 0.8 s user, 0.6 s system and 0.6 s waiting on pages in a period
// is S 60 % against a normal 4 %: 56 points.
func TestExcessFromAirRamp(t *testing.T) {
	j := Job{DUserNS: 8e8, DSysNS: 6e8, DPageWaitNS: 6e8, NormalShare: 0.04}
	if e := Excess([]Job{j}, 2e9); math.Abs(e-56) > 1e-9 {
		t.Errorf("excess %v, want 56", e)
	}
	// Weighted by time: a calm job with three times the time pulls it to a quarter.
	calm := Job{DUserNS: 6e9, NormalShare: 0.04}
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
	idle := Job{DUserNS: 5e6, DSysNS: 20e6, NormalShare: 0.04}
	for i := range 30 {
		if l := f.step(c, true, idle, idle); l != Green {
			t.Fatalf("sample %d: %v, want green", i, l)
		}
	}
	if f.d.Excess != 0 {
		t.Errorf("excess %v with no job at work, want 0", f.d.Excess)
	}
}

// 15 points of excess are yellow only with the paging gate open: system time without paging is
// not memory.
func TestYellowNeedsGate(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	for i := range 10 {
		if l := f.step(c, false, job(0.40, 0.25)); l != Green {
			t.Fatalf("gate closed, sample %d: %v, want green", i, l)
		}
	}
	f.step(c, true, job(0.40, 0.25))
	if l := f.step(c, true, job(0.40, 0.25)); l != Yellow {
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

// One sample at 38 points, then calm: the bucket holds 1 and drains; never red.
func TestSpikeDoesNotRed(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	f.step(c, true, job(0.63, 0.25))
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

// A sleep between two samples (the sleep-inclusive clock moved 600 s, the awake one 2 s) is a
// gap: unknown, whatever the deltas say, and the bucket stays as it was.
func TestGapIsUnknown(t *testing.T) {
	c := Defaults()
	f := newFeed(c)
	for range 3 {
		f.step(c, true, job(0.60, 0.04))
	}
	bucket := f.d.Bucket
	cur := f.prev
	cur.Host.WallNS += 600e9
	cur.Host.AwakeNS += 2e9
	cur.Host.Decomp += 1e6
	cur.Jobs = []Job{job(0.60, 0.04)}
	if !memsample.Gap(f.prev.Host, cur.Host) {
		t.Fatal("the fixture is not a gap")
	}
	if l := f.d.Advance(f.prev, cur, c); l != Unknown || f.d.Bucket != bucket {
		t.Errorf("after a sleep: %v, bucket %v; want unknown, bucket %v", l, f.d.Bucket, bucket)
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

func jobs() []Job {
	return []Job{
		{Hold: "1", ArrivalNS: 1, FootprintMiB: 8192, Victim: true},
		{Hold: "5", ArrivalNS: 5, FootprintMiB: 307, Victim: true},
		{Hold: "6", ArrivalNS: 6, FootprintMiB: 410, Victim: true},
		{Hold: "7", ArrivalNS: 7, FootprintMiB: 512, Victim: true},
	}
}

// Victims go newest first until their footprints cover the deficit; the oldest never goes.
func TestStopListNewestFirstCumulative(t *testing.T) {
	if got := StopList(jobs(), 1024); !slices.Equal(got, []string{"7", "6", "5"}) {
		t.Errorf("stop list %v, want [7 6 5]", got)
	}
	if got := StopList(jobs(), 1<<20); !slices.Equal(got, []string{"7", "6", "5"}) {
		t.Errorf("stop list for a deficit nobody covers %v, want [7 6 5]", got)
	}
}

// A round stops at least one job, even with no deficit.
func TestStopListAtLeastOne(t *testing.T) {
	if got := StopList(jobs(), 0); !slices.Equal(got, []string{"7"}) {
		t.Errorf("stop list %v, want [7]", got)
	}
}

// With only the oldest job and jobs that may not be stopped left, the round lists nothing.
func TestStopListOnlyOldestLeft(t *testing.T) {
	js := jobs()
	for i := range js[1:] {
		js[i+1].Victim = false
	}
	if got := StopList(js, 1024); len(got) != 0 {
		t.Errorf("stop list %v, want none", got)
	}
	if got := StopList(jobs()[:1], 1024); len(got) != 0 {
		t.Errorf("stop list of the oldest alone %v, want none", got)
	}
}
