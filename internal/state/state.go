// Package state is the pure state machine of the protocol: election, observed freshness,
// clamping, undeclared resources and capacity planning. It performs no I/O and reads no clock;
// time is always a parameter.
package state

import (
	"cmp"
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
		t := ahead[i]
		if t.Dead || obs.Stalled(t.Key, now) {
			continue
		}
		return false
	}
	return true
}

// Effective computes each requested declared resource's effective range (PROTOCOL.md section
// 12.2). A resource missing from sizes is undeclared (D12): it is excluded from eff and listed,
// sorted, in undeclared. When min exceeds the size the range becomes size..size (D11) and
// clamped records the requested min; otherwise max is lowered to the size.
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
// free = size - locked. It takes min(max, free) of each resource and is not ok, with a nil take,
// when any free is below that resource's min.
func Plan(eff map[string]spec.Range, sizes, locked map[string]int) (take map[string]int, ok bool) {
	take = make(map[string]int, len(eff))
	for name, r := range eff {
		free := sizes[name] - locked[name]
		if free < r.Min {
			return nil, false
		}
		take[name] = min(r.Max, free)
	}
	return take, true
}
