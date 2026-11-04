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

// A red round keeps a job only when its label knows its typical CPU time, its progress is below 1,
// its predicted remaining time is under the typical run time, and, once a round kept it, the round
// comes before the deadline the first keep set (its time plus its estimate) and the estimate is
// smaller than the one last recorded.
// Mutations, each reddening its row: dropping the TypicalCPUS test keeps "too few runs"; letting
// progress reach 1 keeps "done"; t3 <= T keeps "estimate at T"; dropping the clip loses "stall
// clipped"; dropping the deadline keeps "past the deadline"; dropping the monotone test keeps
// "not shrinking" and "growing"; a deadline not set at the first keep fails "first time".
func TestKeeps(t *testing.T) {
	with := func(f func(*Job)) Job { j := near("v", 2); f(&j); return j }
	same, _, _ := Estimate(near("v", 2))
	const now = int64(100e9)
	before := Kept{DeadlineNS: now + 1e9, LeftS: 12}
	for _, c := range []struct {
		name     string
		j        Job
		rec      Kept
		seen     bool
		keep     bool
		leftS    float64
		deadline int64
	}{
		{"first time", near("v", 2), Kept{}, false, true, 10, now + 10e9},
		{"too few runs for a typical CPU time", with(func(j *Job) { j.TypicalCPUS = 0 }), Kept{}, false, false, 0, 0},
		{"done by its CPU time", with(func(j *Job) { j.CPUNS = 10e9 }), Kept{}, false, false, 0, 0},
		{"early, estimate over T", with(func(j *Job) { j.CPUNS, j.StallEWMA = 1e9, 0.2 }), Kept{}, false, false, 0, 0},
		{"estimate at T", with(func(j *Job) { j.CPUNS, j.StallEWMA = 5e9, 0.5 }), Kept{}, false, false, 0, 0},
		{"stalling, estimate under T", with(func(j *Job) { j.StallEWMA = 0.5 }), Kept{}, false, true, 20, now + 20e9},
		{"stall clipped", with(func(j *Job) { j.TypicalS, j.CPUNS, j.StallEWMA = 100, 9e9, 0.95 }), Kept{}, false, true, 50, now + 50e9},
		{"shrinking, before the deadline", near("v", 2), before, true, true, 10, now + 1e9},
		{"past the deadline", near("v", 2), Kept{DeadlineNS: now, LeftS: 12}, true, false, 0, 0},
		{"not shrinking", near("v", 2), Kept{DeadlineNS: now + 1e9, LeftS: same}, true, false, 0, 0},
		{"growing", near("v", 2), Kept{DeadlineNS: now + 1e9, LeftS: 8}, true, false, 0, 0},
	} {
		k, keep := Keeps(c.j, c.rec, c.seen, now)
		if keep != c.keep {
			t.Errorf("%s: keep %v, want %v", c.name, keep, c.keep)
			continue
		}
		if keep && (math.Abs(k.LeftS-c.leftS) > 1e-6 || math.Abs(float64(k.DeadlineNS-c.deadline)) > 1e3 || k.RoundNS != now) {
			t.Errorf("%s: record %+v; want %.0f s left, deadline %d, round %d", c.name, k, c.leftS, c.deadline, now)
		}
	}
}

// A red round passes over a nearly finished newest job only when the older victim it then stops
// has done less CPU work; otherwise it stops the newest as before. A kept job with no older victim
// behind it is kept and the round lists nobody. A floor round keeps nothing. Records are kept for
// the next round, whatever it lists. Mutations: dropping the CPU comparison lists 2 in "older did
// more"; applying the rule at the floor lists nothing there.
func TestStopListKeepsNearlyFinished(t *testing.T) {
	old := near("1", 1)
	young := func(cpuNS int64) Job { j := near("2", 2); j.CPUNS, j.StallEWMA = cpuNS, 0.2; return j } // no estimate under T
	for _, c := range []struct {
		name  string
		jobs  []Job
		floor bool
		stop  []string
		kept  []string
	}{
		{"older did less", []Job{old, young(1e9), near("3", 3)}, false, []string{"2"}, []string{"3"}},
		{"older did more", []Job{old, young(9e9), near("3", 3)}, false, []string{"3"}, nil},
		{"older did as much", []Job{old, young(8e9), near("3", 3)}, false, []string{"3"}, nil},
		{"nobody behind it", []Job{old, near("3", 3)}, false, nil, []string{"3"}},
		{"floor", []Job{old, young(1e9), near("3", 3)}, true, []string{"3"}, nil},
	} {
		stop, kept := StopList(c.jobs, c.floor, 100, map[string]Kept{"other": {LeftS: 1}}, 77)
		var names []string
		for h := range kept {
			if h != "other" {
				names = append(names, h)
			}
		}
		if !slices.Equal(stop, c.stop) || !slices.Equal(names, c.kept) || kept["other"].LeftS != 1 {
			t.Errorf("%s: stop %v, kept %v; want stop %v, kept %v and the other record", c.name, stop, kept, c.stop, c.kept)
		}
	}
}

// A record lasts as long as its hold is live, whether or not its run ticked. Mutation: pruning by
// the running jobs instead drops a hold that missed a tick.
func TestPruneKeepsLiveHolds(t *testing.T) {
	got := Prune(map[string]Kept{"live": {}, "gone": {}}, []string{"live", "other"})
	if _, ok := got["live"]; !ok || len(got) != 1 {
		t.Errorf("pruned to %v, want only live", got)
	}
	if got := Prune(map[string]Kept{"gone": {}}, nil); got != nil {
		t.Errorf("pruned to %v, want none", got)
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
