package watch

import (
	"cmp"
	"slices"
)

// StopList names the holds a stop round stops: victims whose child still runs, newest arrival
// first, never the oldest live job, until their footprints sum to max(deficitMiB, 1).
func StopList(jobs []Job, deficitMiB int64) []string {
	newest := slices.Clone(jobs)
	slices.SortStableFunc(newest, func(a, b Job) int {
		return cmp.Or(cmp.Compare(b.ArrivalNS, a.ArrivalNS), cmp.Compare(b.Hold, a.Hold))
	})
	var stop []string
	var freed int64
	for _, j := range newest[:max(len(newest)-1, 0)] {
		if freed >= max(deficitMiB, 1) {
			break
		}
		if j.Victim && !j.ChildExited {
			stop = append(stop, j.Hold)
			freed += j.FootprintMiB
		}
	}
	return stop
}
