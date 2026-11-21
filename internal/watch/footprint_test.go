package watch

import (
	"encoding/binary"
	"math"
	"slices"
	"testing"
)

// sized is j with its tree at mib.
func sized(j Job, mib int64) Job {
	j.FootprintMiB = mib
	return j
}

// ms is a duration in ms, as ns.
func ms(v float64) int64 { return int64(v * 1e6) }

// excessAll is E over every counted job, the floor left out: the mean the floor may only lower.
func excessAll(jobs []Job, periodNS int64) float64 {
	var sum, weight float64
	for _, j := range jobs {
		w := float64(j.DUserNS + j.DSysNS + j.DPageWaitNS)
		if s, ok := Share(j); ok && j.Learned && w >= minWorkShare*float64(periodNS) {
			sum += w * max(0, s-j.NormalShare)
			weight += w
		}
	}
	if weight == 0 {
		return 0
	}
	return 100 * sum / weight
}

// small24 is a learned tree of 24 MiB that spends a tick of userMS and sysMS (pageMS waiting on
// pages) against its label's normal share.
func small24(userMS, sysMS, pageMS, normal float64) Job {
	return Job{DUserNS: ms(userMS), DSysNS: ms(sysMS), DPageWaitNS: ms(pageMS), NormalShare: normal,
		Learned: true, Victim: true, FootprintMiB: 24}
}

// Two learned trees of 24 MiB that spent a 2 s tick almost wholly in the kernel, as recorded at a
// red round that stopped one of them, add no excess. Mutation: return the mean over every counted
// job alone → 67.7.
func TestExcessSmallTreesTick(t *testing.T) {
	jobs := []Job{small24(16.5, 1595.1, 0, 0.41), small24(13.3, 1443.6, 0, 0.207)}
	if e := Excess(jobs, 2e9); e != 0 {
		t.Errorf("excess %v, want 0 (all counted: %.1f)", e, excessAll(jobs, 2e9))
	}
}

// A tree counts from 128 MiB on. Mutations: > for >= reddens the 128 row; a floor of 64 reddens the
// 100 row.
func TestExcessFootprintFloor(t *testing.T) {
	for _, c := range []struct {
		mib  int64
		want float64
	}{{100, 0}, {127, 0}, {128, 79}} {
		if e := Excess([]Job{sized(job(0.99, 0.2), c.mib)}, 2e9); math.Abs(e-c.want) > 1e-6 {
			t.Errorf("%d MiB: excess %v, want %v", c.mib, e, c.want)
		}
	}
}

// A small tree's time still weighs: beside a 2 GiB tree in excess, a 99 MiB tree with 10.7 s of
// user time keeps E at the mean over both. Mutation: return the large trees' mean alone → 56.
func TestExcessFloorNeverRaisesE(t *testing.T) {
	jobs := []Job{sized(job(0.6, 0.04), 2048),
		{DUserNS: 10.7e9, NormalShare: 0.1, Learned: true, FootprintMiB: 99}}
	if e, want := Excess(jobs, 2e9), 100*2e9*0.56/12.7e9; math.Abs(e-want) > 1e-9 {
		t.Errorf("excess %v, want the mean over both, %v", e, want)
	}
}

// A small busy tree does not hide a large tree's own stall: E is the large tree's 30, not the 11.5
// a small tree with no excess but its weight would leave. Mutation: count small trees with no
// excess but their weight → 11.5.
func TestExcessSmallTreeHidesNoStall(t *testing.T) {
	big := Job{DUserNS: 0.5e9, DSysNS: 0.5e9, NormalShare: 0.2, Learned: true, FootprintMiB: 2048}
	small := small24(16, 1584, 0, 0.2)
	if e := Excess([]Job{big, small}, 2e9); math.Abs(e-30) > 1e-9 {
		t.Errorf("excess %v, want 30", e)
	}
}

// A footprint of 0 with time is a tree under 1 MiB, under the floor like any small tree. Mutation:
// exempt 0 from the floor → 79.
func TestExcessZeroFootprintIsSmall(t *testing.T) {
	j := Job{DUserNS: ms(2.2), DSysNS: ms(217.8), NormalShare: 0.2, Learned: true}
	if e := Excess([]Job{j}, 2e9); e != 0 {
		t.Errorf("excess %v, want 0", e)
	}
}

// The recorded ticks of two learned trees of 24 MiB, 13 samples with the paging gate open in all
// but two: v0.9.1 went red at the 13th and stopped one of them. They leave the level green and the
// bucket empty. Mutation: drop the floor → the bucket fills from the first sample, red at the 13th.
func TestSmallTreesNeverRed(t *testing.T) {
	ticks := []struct {
		gate                         bool
		mUser, mSys, pUser, pSys, pW float64
	}{
		{true, 10.3, 1245.5, 125.8, 1129.7, 5.5},
		{true, 12.1, 1364.9, 10.3, 1244.7, 0},
		{true, 12.7, 1480.4, 11.7, 1307.5, 0},
		{true, 11.6, 1404.2, 12.2, 1425.7, 0},
		{true, 10.4, 1422.2, 12.3, 1436.0, 0},
		{false, 10.4, 1526.3, 10.6, 1459.7, 0},
		{false, 10.7, 1525.9, 10.5, 1515.8, 0},
		{true, 14.5, 1449.4, 10.9, 1534.8, 0},
		{true, 13.6, 1440.0, 14.5, 1457.0, 0},
		{true, 11.3, 1403.5, 13.7, 1406.6, 0},
		{true, 12.3, 1370.9, 11.4, 1385.2, 0},
		{true, 13.2, 1448.4, 12.8, 1389.1, 0},
		{true, 16.5, 1595.1, 13.3, 1443.6, 0},
	}
	c := Defaults()
	f := newFeed(c)
	for i, k := range ticks {
		if l := f.step(c, k.gate, small24(k.mUser, k.mSys, 0, 0.41), small24(k.pUser, k.pSys, k.pW, 0.207)); l != Green || f.d.Bucket != 0 {
			t.Fatalf("sample %d: %v, bucket %v; want green and empty", i+1, l, f.d.Bucket)
		}
	}
}

// A calibration-shaped thrash: an older learning observer of 1 MiB and a newer learned hog of 5 GiB
// in excess, gate open: red at the tenth sample, and the round lists the hog. Mutation: a floor
// above 5 GiB → never red.
func TestLargeHogStillRed(t *testing.T) {
	observer := Job{Hold: "obs", ArrivalNS: 1, FootprintMiB: 1, Victim: true, DUserNS: 1e7}
	hog := sized(job(0.72, 0.003), 5120)
	hog.Hold, hog.ArrivalNS, hog.Victim = "hog", 2, true
	c := Defaults()
	f := newFeed(c)
	var l Level
	for i := 1; i <= 10; i++ {
		if l = f.step(c, true, observer, hog); i < 10 && l == Red {
			t.Fatalf("red at sample %d, want the tenth", i)
		}
	}
	if stop := stopList([]Job{observer, hog}, false, 0); l != Red || !slices.Equal(stop, []string{"hog"}) {
		t.Errorf("tenth sample %v, stop list %v; want red and [hog]", l, stop)
	}
}

// A small tree beside a large tree whose own excess is 25 leaves the level yellow, as in v0.9.1, at
// any weights. Mutation: zero E whenever a counted job is under the floor → green.
func TestYellowWithSmallAndLargeTrees(t *testing.T) {
	big := sized(job(0.45, 0.2), 2048)
	for _, small := range []Job{small24(20, 1980, 0, 0.2), small24(100, 9900, 0, 0.2)} {
		c := Defaults()
		f := newFeed(c)
		var l Level
		for range 3 {
			l = f.step(c, true, big, small)
		}
		if l != Yellow {
			t.Errorf("small tree of %d ms: %v, want yellow", (small.DUserNS+small.DSysNS)/1e6, l)
		}
	}
}

// A keep never trades the stop of a large job for a small tree's: with a shortage of 0, a nearly
// finished newest victim of 4 GiB is kept only when the next victim, which did less CPU work,
// reaches 128 MiB. Mutation: drop minFootprintMiB from substitutes → the 24 MiB one is stopped and
// the 4 GiB one kept.
func TestKeepSubstituteReachesFloor(t *testing.T) {
	for _, c := range []struct {
		next       int64
		stop, kept []string
	}{{24, []string{"3"}, nil}, {2048, []string{"2"}, []string{"3"}}} {
		next := sized(near("2", 2), c.next)
		next.CPUNS, next.StallEWMA = 1e9, 0.2 // no estimate under its typical run time
		stop, kept := StopList([]Job{near("1", 1), next, sized(near("3", 3), 4096)}, false, 0, nil, 77)
		var names []string
		for h := range kept {
			names = append(names, h)
		}
		if !slices.Equal(stop, c.stop) || !slices.Equal(names, c.kept) {
			t.Errorf("next victim of %d MiB: stop %v, kept %v; want %v, %v", c.next, stop, names, c.stop, c.kept)
		}
	}
}

// FuzzExcessFloorNeverRaises: for any jobs, E is at most the mean over every counted job.
func FuzzExcessFloorNeverRaises(f *testing.F) {
	f.Add([]byte{0, 0, 0, 30, 0, 0, 7, 208, 0, 0, 0, 0, 20, 1, 0, 0, 0, 0, 0, 1, 0, 0, 0, 200, 0, 0, 0, 0, 40, 1, 8, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		var jobs []Job
		for i := 0; i+16 <= len(data); i += 16 {
			b := data[i : i+16]
			jobs = append(jobs, Job{
				DUserNS:      int64(binary.BigEndian.Uint32(b[0:])) * 1e3,
				DSysNS:       int64(binary.BigEndian.Uint32(b[4:])) * 1e3,
				DPageWaitNS:  int64(binary.BigEndian.Uint16(b[8:])) * 1e3,
				NormalShare:  float64(b[10]) / 255,
				Learned:      b[11]%4 != 0,
				FootprintMiB: int64(binary.BigEndian.Uint32(b[12:]) % 4096),
			})
		}
		if e, all := Excess(jobs, 2e9), excessAll(jobs, 2e9); e > all+1e-9 || e < 0 {
			t.Fatalf("excess %v above the mean over every counted job %v: %+v", e, all, jobs)
		}
	})
}
