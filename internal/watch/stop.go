package watch

import (
	"cmp"
	"slices"
)

// StopList names the holds a stop round stops: victims, newest arrival first, never the oldest live
// job, until their footprints sum to max(shortMiB, 1). A red round takes learned victims only; a
// floor round takes every learned one first, then the learning ones.
func StopList(jobs []Job, floor bool, shortMiB int64) []string {
	newest := slices.Clone(jobs)
	slices.SortStableFunc(newest, func(a, b Job) int {
		return cmp.Or(cmp.Compare(b.ArrivalNS, a.ArrivalNS), cmp.Compare(b.Hold, a.Hold))
	})
	var stop []string
	var freed int64
	for _, learned := range []bool{true, false} {
		for _, j := range newest[:max(len(newest)-1, 0)] {
			if freed >= max(shortMiB, 1) {
				return stop
			}
			if j.Victim && j.Learned == learned && (learned || floor) {
				stop = append(stop, j.Hold)
				freed += j.FootprintMiB
			}
		}
	}
	return stop
}
