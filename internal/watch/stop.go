package watch

import (
	"cmp"
	"maps"
	"math"
	"slices"
)

// The rule that keeps a nearly finished job in a red round. Not calibrated on records: a
// simulator calibrated on them chose it.
const (
	// StallTauS is the time constant, in seconds, of a job's smoothed stall share.
	StallTauS = 10
	// maxStall bounds the smoothed stall share in the estimate, so that a job that stalls almost
	// wholly is not predicted to need forever.
	maxStall = 0.8
)

// Kept is what a red round recorded of a job it kept because it is nearly finished: the host time
// by which its first keep predicted it to end, the predicted remaining seconds and the progress at
// the round that last kept it, and that round's host time.
type Kept struct {
	DeadlineNS int64   `json:"deadline_ns"`
	LeftS      float64 `json:"left_s"`
	Done       float64 `json:"done"`
	RoundNS    int64   `json:"round_ns"`
}

// Smooth is the stall share smoothed over StallTauS: s alone for the first value (first true),
// else prev moved towards s for a period of periodS seconds.
func Smooth(prev, s float64, first bool, periodS float64) float64 {
	if first {
		return s
	}
	return prev + (1-math.Exp(-periodS/StallTauS))*(s-prev)
}

// Estimate is the job's predicted remaining run time in seconds and its progress, its CPU time so
// far over its label's typical CPU time: its label's typical run time less the part its progress
// covers, slowed by its smoothed stall share, at most maxStall. ok is false when the label has no
// typical CPU time (fewer than 10 runs report one) or the progress has reached 1. A progress of 0
// or a typical run time of 0 gives an estimate of at least the typical run time, which Keeps
// never keeps.
func Estimate(j Job) (leftS, done float64, ok bool) {
	if j.TypicalCPUS <= 0 {
		return 0, 0, false
	}
	done = float64(j.CPUNS) / 1e9 / j.TypicalCPUS
	if done >= 1 {
		return 0, done, false
	}
	return float64(j.TypicalS) * (1 - done) / (1 - min(max(j.StallEWMA, 0), maxStall)), done, true
}

// Keeps reports whether a red round at host time nowNS may keep j as nearly finished, and the
// record it then writes: j's estimate ends within its label's typical run time and, when an
// earlier round kept it (seen, with rec), the round comes before the deadline that first keep set
// and the estimate is smaller than the one last recorded. The first keep sets the deadline to
// nowNS plus its estimate.
func Keeps(j Job, rec Kept, seen bool, nowNS int64) (Kept, bool) {
	left, done, ok := Estimate(j)
	switch {
	case !ok || left >= float64(j.TypicalS):
		return rec, false
	case !seen:
		rec.DeadlineNS = nowNS + int64(left*1e9)
	case nowNS >= rec.DeadlineNS || left >= rec.LeftS:
		return rec, false
	}
	rec.LeftS, rec.Done, rec.RoundNS = left, done, nowNS
	return rec, true
}

// StopList names the holds a stop round at host time atNS stops: victims, newest arrival first,
// never the oldest live job, until their footprints sum to max(shortMiB, 1). A red round takes
// learned victims only, and passes over one that Keeps with its record in kept, unless the older
// victim it would stop instead has done at least as much CPU work; a floor round keeps nothing
// and takes every learned victim first, then the learning ones. It returns kept with the records
// of the jobs it kept.
func StopList(jobs []Job, floor bool, shortMiB int64, kept map[string]Kept, atNS int64) ([]string, map[string]Kept) {
	newest := slices.Clone(jobs)
	slices.SortStableFunc(newest, func(a, b Job) int {
		return cmp.Or(cmp.Compare(b.ArrivalNS, a.ArrivalNS), cmp.Compare(b.Hold, a.Hold))
	})
	newest = newest[:max(len(newest)-1, 0)] // never the oldest
	var stop []string
	var freed int64
	for _, learned := range []bool{true, false} {
		if !learned && !floor {
			break
		}
		cands := slices.DeleteFunc(slices.Clone(newest), func(j Job) bool { return !j.Victim || j.Learned != learned })
		for i, j := range cands {
			if freed >= max(shortMiB, 1) {
				break
			}
			if !floor {
				prev, seen := kept[j.Hold]
				if rec, ok := Keeps(j, prev, seen, atNS); ok && (i+1 == len(cands) || cands[i+1].CPUNS < j.CPUNS) {
					kept = maps.Clone(kept)
					if kept == nil {
						kept = map[string]Kept{}
					}
					kept[j.Hold] = rec
					continue
				}
			}
			stop = append(stop, j.Hold)
			freed += j.FootprintMiB
		}
	}
	return stop, kept
}

// Prune drops the records of the holds that are no longer live: a record lasts as long as its
// hold, also while the hold's run misses a tick.
func Prune(kept map[string]Kept, live []string) map[string]Kept {
	maps.DeleteFunc(kept, func(hold string, _ Kept) bool { return !slices.Contains(live, hold) })
	if len(kept) == 0 {
		return nil
	}
	return kept
}
