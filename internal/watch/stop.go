package watch

import (
	"cmp"
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

// Kept is a job a red round kept because it is nearly finished: its predicted remaining seconds
// and its progress at that round, and the round's host time.
type Kept struct {
	LeftS   float64 `json:"left_s"`
	Done    float64 `json:"done"`
	RoundNS int64   `json:"round_ns"`
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
// far over its label's typical CPU time. A job's nominal remaining time is its label's typical run
// time less the part its progress covers; it runs slower by its smoothed stall share, at most
// maxStall. ok is false when the job has no typical CPU time or run time, or its progress is not
// strictly between 0 and 1.
func Estimate(j Job) (leftS, done float64, ok bool) {
	if j.TypicalCPUS <= 0 || j.TypicalS <= 0 {
		return 0, 0, false
	}
	done = float64(j.CPUNS) / 1e9 / j.TypicalCPUS
	if !(done > 0 && done < 1) {
		return 0, done, false
	}
	return float64(j.TypicalS) * (1 - done) / (1 - min(max(j.StallEWMA, 0), maxStall)), done, true
}

// Keeps reports whether a red round keeps j, and what it records: j has an estimate that ends
// within its label's typical run time and, when an earlier red round kept it (seen, with prev),
// that is smaller than the one that round recorded.
func Keeps(j Job, prev Kept, seen bool) (Kept, bool) {
	left, done, ok := Estimate(j)
	if !ok || left >= float64(j.TypicalS) || seen && left >= prev.LeftS {
		return Kept{}, false
	}
	return Kept{LeftS: left, Done: done}, true
}

// StopList names the holds a stop round at host time atNS stops: victims, newest arrival first,
// never the oldest live job, until their footprints sum to max(shortMiB, 1). A red round takes
// learned victims only, and passes over one that Keeps with its record in kept; a floor round
// keeps nothing and takes every learned victim first, then the learning ones. It also returns the
// records to keep for the next round: those of kept for the jobs still running and not listed,
// and those of the jobs it kept.
func StopList(jobs []Job, floor bool, shortMiB int64, kept map[string]Kept, atNS int64) ([]string, map[string]Kept) {
	newest := slices.Clone(jobs)
	slices.SortStableFunc(newest, func(a, b Job) int {
		return cmp.Or(cmp.Compare(b.ArrivalNS, a.ArrivalNS), cmp.Compare(b.Hold, a.Hold))
	})
	next := map[string]Kept{}
	for _, j := range jobs {
		if k, ok := kept[j.Hold]; ok {
			next[j.Hold] = k
		}
	}
	var stop []string
	var freed int64
	for _, learned := range []bool{true, false} {
		for _, j := range newest[:max(len(newest)-1, 0)] {
			if freed >= max(shortMiB, 1) {
				break
			}
			if !j.Victim || j.Learned != learned || !learned && !floor {
				continue
			}
			if prev, seen := kept[j.Hold]; !floor {
				if k, keep := Keeps(j, prev, seen); keep {
					k.RoundNS = atNS
					next[j.Hold] = k
					continue
				}
			}
			stop = append(stop, j.Hold)
			delete(next, j.Hold)
			freed += j.FootprintMiB
		}
	}
	if len(next) == 0 {
		next = nil
	}
	return stop, next
}
