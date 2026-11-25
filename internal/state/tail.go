package state

import "sort"

// A trimmed hold's reservation follows its run's footprint: the largest of its last readings plus
// a margin, never above the need it asked for. These are the window and the margin.
const (
	TrimWindowS   = 60  // seconds of readings a trimmed hold's reservation looks back over
	MarginMinMiB  = 256 // the least kept reserved above recent use
	MarginDivisor = 10  // ... or this share of the need asked, whichever is larger
)

// Margin is what a trimmed hold keeps reserved above its recent footprint, in MiB:
// max(MarginMinMiB, askedMiB/MarginDivisor).
func Margin(askedMiB int64) int64 { return max(MarginMinMiB, askedMiB/MarginDivisor) }

// Tail is a window over a run's footprint readings, one per tree listing that succeeded, and the
// reservation they give. The zero value is not ready; use NewTail. A Tail is not safe for
// concurrent use.
type Tail struct {
	asked    int64
	readings []int64 // a ring of the last len(readings) readings
	next     int     // where the next reading goes
	n        int     // readings added, capped at len(readings)
}

// NewTail returns a window of window readings (at least 1; a smaller value spans 1) for a hold
// that asked for askedMiB.
func NewTail(askedMiB int64, window int) *Tail {
	return &Tail{asked: askedMiB, readings: make([]int64, max(1, window))}
}

// Add records one reading that succeeded, in MiB. A listing that failed is not a reading: the
// caller adds nothing for it.
func (t *Tail) Add(footprintMiB int64) {
	t.readings[t.next] = footprintMiB
	t.next = (t.next + 1) % len(t.readings)
	t.n = min(t.n+1, len(t.readings))
}

// Need is what the hold reserves: the need asked until the window holds as many readings as it
// spans, then min(asked, recent + Margin(asked)), recent the largest reading in the window, the
// newest included.
func (t *Tail) Need() int64 {
	if t.n < len(t.readings) {
		return t.asked
	}
	recent := t.readings[0]
	for _, r := range t.readings[1:] {
		recent = max(recent, r)
	}
	m := Margin(t.asked)
	if recent > t.asked-m { // recent + m would reach the need asked, or overflow
		return t.asked
	}
	return recent + m
}

// A learned run's reservation follows its footprint over a window learned per label (PROTOCOL 12.5
// and 20): 1.5 times the longest gap its label's runs recorded, at least TailFloorS seconds,
// TailDefaultS while fewer than TailMinWindows runs recorded one, never more than TailMaxWindowS.
// None is configurable.
const (
	TailFactorNum, TailFactorDen = 3, 2
	TailFloorS                   = 120
	TailDefaultS                 = 600
	TailMinWindows               = 3
	TailMaxWindowS               = 86400 // a day
)

// Shorten makes the window span window readings (at least 1) when that is fewer than it spans,
// keeping the newest readings it holds. It never lengthens the window.
func (t *Tail) Shorten(window int) {
	window = max(1, window)
	if window >= len(t.readings) {
		return
	}
	k := min(t.n, window)
	kept := make([]int64, window)
	for i := range k { // the newest k readings, oldest first
		kept[i] = t.readings[(t.next-k+i+len(t.readings))%len(t.readings)]
	}
	t.readings, t.next, t.n = kept, k%window, k
}

// Gap is the window, in readings, that would never have reserved less than a later reading of
// the series, for a hold that asked for askedMiB (PROTOCOL 20, mem_window_s): with m =
// Margin(askedMiB) and readings numbered from 1, for each reading j that has a later one, F the
// largest later reading and u the last reading at or before j that is at least F - m (0 when
// none), the largest j - u + 1. ok is false under two readings. O(n log n).
func Gap(readings []int32, askedMiB int64) (n int, ok bool) {
	if len(readings) < 2 {
		return 0, false
	}
	m := Margin(askedMiB)
	last := len(readings) - 1
	later := make([]int64, last) // later[j]: the largest reading after j (0-based)
	later[last-1] = int64(readings[last])
	for j := last - 2; j >= 0; j-- {
		later[j] = max(int64(readings[j+1]), later[j+1])
	}
	var stack []int // 0-based indices whose readings strictly decrease from bottom to top
	for j := range last {
		for len(stack) > 0 && readings[stack[len(stack)-1]] <= readings[j] {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, j)
		need := later[j] - m
		// The stack's readings that are at least need form a prefix: the topmost of it is u.
		k := sort.Search(len(stack), func(i int) bool { return int64(readings[stack[i]]) < need })
		u := 0 // 1-based; 0 when no reading at or before j is at least need
		if k > 0 {
			u = stack[k-1] + 1
		}
		n = max(n, j+1-u+1)
	}
	return n, true
}
