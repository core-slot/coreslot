package state

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
// spans, then max(1, min(asked, recent + Margin(asked))), recent the largest reading in the
// window, the newest included.
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
		return max(1, t.asked)
	}
	return max(1, recent+m)
}
