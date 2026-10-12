// Package state is the pure state machine of the protocol: election, observed freshness,
// clamping, undeclared resources and capacity planning. It performs no I/O and reads no clock;
// time is always a parameter.
package state

import (
	"cmp"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// Key orders tickets: ascending OrderKey, ties by PID, then Seq (PROTOCOL.md section 9).
type Key struct {
	OrderKey uint64
	PID, Seq int
}

// Less reports whether k is ahead of o.
func (k Key) Less(o Key) bool {
	if c := cmp.Compare(k.OrderKey, o.OrderKey); c != 0 {
		return c < 0
	}
	if c := cmp.Compare(k.PID, o.PID); c != 0 {
		return c < 0
	}
	return k.Seq < o.Seq
}

// Ticket is one queue entry as the caller read it. Dead means its lock probe succeeded.
type Ticket struct {
	Key  Key
	Need map[string]spec.Range
	Dead bool
}

// Ahead is the election (PROTOCOL.md section 10.3). It scans the tickets ahead of me from the
// nearest, skipping dead and STALLED ones; the first live, non-stalled ticket ends the scan and
// me is not the head. It reports whether me would be the head. The caller feeds obs with the
// tickets' mtimes beforehand and passes the observer's monotonic time as now.
func Ahead(tickets []Ticket, me Key, obs *Observer, now time.Duration) (wouldBeHead bool) {
	ahead := make([]Ticket, 0, len(tickets))
	for _, t := range tickets {
		if t.Key.Less(me) {
			ahead = append(ahead, t)
		}
	}
	slices.SortFunc(ahead, func(a, b Ticket) int {
		switch {
		case a.Key.Less(b.Key):
			return -1
		case b.Key.Less(a.Key):
			return 1
		}
		return 0
	})
	// Nearest first: the ticket just ahead of me is the last in ascending order.
	for i := len(ahead) - 1; i >= 0; i-- {
		if PassedOver(ahead[i], obs, now) {
			continue
		}
		return false
	}
	return true
}

// PassedOver reports whether the election skips t: it is dead or, for obs at now, STALLED. The
// first ticket ahead, from the nearest, that is not passed over ends the election.
func PassedOver(t Ticket, obs *Observer, now time.Duration) bool {
	return t.Dead || obs.Stalled(t.Key, now)
}

// Effective computes each requested declared resource's effective range (PROTOCOL.md section 12.2).
// A resource missing from sizes is undeclared (section 12.1): it is excluded from eff and listed,
// sorted, in undeclared. When min exceeds the size the range becomes size..size and clamped records
// the requested min; otherwise max is lowered to the size.
func Effective(need map[string]spec.Range, sizes map[string]int) (eff map[string]spec.Range, clamped map[string]int, undeclared []string) {
	eff = make(map[string]spec.Range, len(need))
	clamped = make(map[string]int)
	for name, r := range need {
		size, ok := sizes[name]
		if !ok {
			undeclared = append(undeclared, name)
			continue
		}
		if r.Min > size {
			clamped[name] = r.Min
			r = spec.Range{Min: size, Max: size}
		} else if r.Max > size {
			r.Max = size
		}
		eff[name] = r
	}
	sort.Strings(undeclared)
	return eff, clamped, undeclared
}

// Plan sizes an admission (PROTOCOL.md section 12.1 steps 4 to 6). locked counts the locked
// tokens of each resource at every index, including at or above the size (I3), so
// free = size - locked. It is not ok, with a nil take, when any free is below that resource's
// min. Otherwise take holds every effective min (the minimum pass) and then, in ascending
// resource name, further tokens up to min(max, free) per resource while the grant holds fewer
// than spec.MaxPerGrant tokens in total (the extra pass), so the cap never starves a later
// resource below its minimum.
func Plan(eff map[string]spec.Range, sizes, locked map[string]int) (take map[string]int, ok bool) {
	free := make(map[string]int, len(eff))
	mins := make(map[string]int, len(eff))
	for name, r := range eff {
		free[name] = sizes[name] - locked[name]
		if free[name] < r.Min {
			return nil, false
		}
		mins[name] = r.Min
	}
	return fill(eff, free, mins), true
}

// AdoptPlan sizes an adoption (PROTOCOL.md section 12.4), which has no minimum: the minimum pass
// takes min(min, free) of each resource (free = size - locked, never below 0), and the extra
// pass takes further tokens up to min(max, free) per resource, in ascending resource name,
// while the grant holds fewer than spec.MaxPerGrant tokens in total. The caller reports
// max(0, requested min - take) as the shortfall.
func AdoptPlan(eff map[string]spec.Range, sizes, locked map[string]int) (take map[string]int) {
	free := make(map[string]int, len(eff))
	mins := make(map[string]int, len(eff))
	for name, r := range eff {
		free[name] = max(sizes[name]-locked[name], 0)
		mins[name] = min(r.Min, free[name])
	}
	return fill(eff, free, mins)
}

// fill starts from the minimum pass's mins and adds the extra pass: in ascending resource name,
// up to min(max, free) per resource while the total stays at most spec.MaxPerGrant.
func fill(eff map[string]spec.Range, free, mins map[string]int) map[string]int {
	take := make(map[string]int, len(eff))
	total := 0
	for name, n := range mins {
		take[name] = n
		total += n
	}
	for _, name := range slices.Sorted(maps.Keys(eff)) {
		room := min(eff[name].Max, free[name]) - take[name]
		add := max(0, min(room, spec.MaxPerGrant-total))
		take[name] += add
		total += add
	}
	return take
}
