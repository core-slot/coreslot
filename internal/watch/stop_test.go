package watch

import (
	"math"
	"slices"
	"testing"
)

// near is a job 80 % through its label's typical 10 CPU seconds, whose label typically runs 50 s:
// with no stall it has about 10 s left.
func near(hold string, arrival int64) Job {
	return Job{Hold: hold, ArrivalNS: arrival, FootprintMiB: 512, Victim: true, Learned: true,
		CPUNS: 8e9, TypicalCPUS: 10, TypicalS: 50}
}

// A red round keeps a job only when its label knows its typical CPU and run time, its progress
// is strictly between 0 and 1, its predicted remaining time is under the typical run time, and,
// once a round kept it, that estimate is smaller than the one that round recorded.
// Mutations, each reddening its row: dropping the TypicalCPUS test keeps "too few runs"; letting
// progress reach 1 keeps "done"; t3 <= T keeps "estimate at T"; dropping the clip loses "stall
// clipped"; dropping the monotone test keeps "not shrinking".
func TestKeeps(t *testing.T) {
	with := func(f func(*Job)) Job { j := near("v", 2); f(&j); return j }
	same, _, _ := Estimate(near("v", 2))
	for _, c := range []struct {
		name  string
		j     Job
		prev  Kept
		seen  bool
		keep  bool
		leftS float64
	}{
		{"nearly finished, first time", near("v", 2), Kept{}, false, true, 10},
		{"too few runs for a typical CPU time", with(func(j *Job) { j.TypicalCPUS = 0 }), Kept{}, false, false, 0},
		{"no typical run time", with(func(j *Job) { j.TypicalS = 0 }), Kept{}, false, false, 0},
		{"no CPU time yet", with(func(j *Job) { j.CPUNS = 0 }), Kept{}, false, false, 0},
		{"done by its CPU time", with(func(j *Job) { j.CPUNS = 10e9 }), Kept{}, false, false, 0},
		{"past its typical CPU time", with(func(j *Job) { j.CPUNS = 12e9 }), Kept{}, false, false, 0},
		{"early, estimate over T", with(func(j *Job) { j.CPUNS, j.StallEWMA = 1e9, 0.2 }), Kept{}, false, false, 0},
		{"estimate at T", with(func(j *Job) { j.CPUNS, j.StallEWMA = 5e9, 0.5 }), Kept{}, false, false, 0},
		{"stalling, estimate under T", with(func(j *Job) { j.StallEWMA = 0.5 }), Kept{}, false, true, 20},
		{"stall clipped", with(func(j *Job) { j.TypicalS, j.CPUNS, j.StallEWMA = 100, 9e9, 0.95 }), Kept{}, false, true, 50},
		{"shrinking", near("v", 2), Kept{LeftS: 12}, true, true, 10},
		{"not shrinking", near("v", 2), Kept{LeftS: same}, true, false, 0},
		{"growing", near("v", 2), Kept{LeftS: 8}, true, false, 0},
	} {
		k, keep := Keeps(c.j, c.prev, c.seen)
		if keep != c.keep || math.Abs(k.LeftS-c.leftS) > 1e-9 {
			t.Errorf("%s: keep %v, left %.3f s; want %v, %.3f s", c.name, keep, k.LeftS, c.keep, c.leftS)
		}
		if keep && math.Abs(k.Done-float64(c.j.CPUNS)/1e9/c.j.TypicalCPUS) > 1e-9 {
			t.Errorf("%s: done %v", c.name, k.Done)
		}
	}
}

// A red round passes over a nearly finished newest job, takes the next one and records the kept
// one with the round's time; the next red round, whose estimate for it did not shrink, lists it
// and drops its record. A floor round keeps nothing. Records of jobs no longer running are
// dropped; one of a running job the round did not reach is carried. Mutations: ignoring the floor
// flag keeps 3 at the floor; never dropping a listed job's record keeps it in the map.
func TestStopListKeepsNearlyFinished(t *testing.T) {
	same, _, _ := Estimate(near("3", 3))
	js := []Job{near("1", 1), near("2", 2), near("3", 3)}
	js[1].CPUNS, js[1].StallEWMA = 1e9, 0.2 // early: its estimate, 56 s, is over T
	stop, kept := StopList(js, false, 100, map[string]Kept{"gone": {LeftS: 1}}, 77)
	if !slices.Equal(stop, []string{"2"}) || len(kept) != 1 || kept["3"].RoundNS != 77 || math.Abs(kept["3"].LeftS-10) > 1e-9 {
		t.Fatalf("first red round: stop %v, kept %v; want [2], 3 kept at 77 with 10 s left", stop, kept)
	}
	stop, kept = StopList(js[:1:1], false, 100, kept, 90) // 3 not running at this round: its record goes
	if len(stop) != 0 || kept != nil {
		t.Errorf("round with the oldest alone: stop %v, kept %v; want none, none", stop, kept)
	}
	js = []Job{near("1", 1), near("3", 3), near("4", 4)}
	js[2].CPUNS, js[2].StallEWMA = 1e9, 0.2
	stop, kept = StopList(js, false, 100, map[string]Kept{"3": {LeftS: same, RoundNS: 77}}, 99)
	if !slices.Equal(stop, []string{"4"}) || kept["3"].RoundNS != 77 {
		t.Errorf("round covered by 4: stop %v, kept %v; want [4], 3's record carried", stop, kept)
	}
	stop, kept = StopList(js[:2], false, 100, kept, 120)
	if !slices.Equal(stop, []string{"3"}) || kept != nil {
		t.Errorf("estimate not shrinking: stop %v, kept %v; want [3], no record", stop, kept)
	}
	stop, kept = StopList([]Job{near("1", 1), near("3", 3)}, true, 100, nil, 130)
	if !slices.Equal(stop, []string{"3"}) || kept != nil {
		t.Errorf("floor round: stop %v, kept %v; want [3], nothing kept", stop, kept)
	}
	stop, kept = StopList([]Job{near("1", 1), near("3", 3), near("5", 5)}, false, 100, nil, 140)
	if len(stop) != 0 || len(kept) != 2 {
		t.Errorf("every victim nearly finished: stop %v, kept %v; want none listed, 3 and 5 kept", stop, kept)
	}
}

// The stall share is smoothed with a time constant of StallTauS: the first value is taken as it
// is, and one 2 s period moves it by 1 − e^(−0.2) of the way. Mutation: a fixed weight of 0.5
// moves it to 0.5.
func TestSmooth(t *testing.T) {
	if got := Smooth(0.7, 0.3, true, 2); got != 0.3 {
		t.Errorf("first value %v, want 0.3", got)
	}
	if got, want := Smooth(0, 1, false, 2), 1-math.Exp(-0.2); math.Abs(got-want) > 1e-12 {
		t.Errorf("one period %v, want %v", got, want)
	}
}
