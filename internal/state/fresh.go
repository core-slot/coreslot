package state

import "time"

// StallAfter is how long a ticket's mtime may stay unchanged, on the observer's own monotonic
// clock, before the observer calls the ticket STALLED (PROTOCOL.md section 10.2).
const StallAfter = 10 * time.Second

// sighting is what an observer remembers of one ticket: the last mtime value it read and the
// observer-clock time at which that value first appeared.
type sighting struct {
	mtime      int64
	lastChange time.Duration
}

// Observer judges freshness by watching tickets change, never by comparing an mtime with a
// clock: the mtime is only an opaque value that either differs from the last one or does not.
// The zero value is ready to use. An Observer is not safe for concurrent use.
type Observer struct {
	seen map[Key]sighting
}

// Observe records that the ticket k currently carries mtime, read at observer time now. A first
// sighting, or a value different from the last one, restarts the ticket's timer; the same value
// leaves it running.
func (o *Observer) Observe(k Key, mtime int64, now time.Duration) {
	if o.seen == nil {
		o.seen = make(map[Key]sighting)
	}
	if s, ok := o.seen[k]; ok && s.mtime == mtime {
		return
	}
	o.seen[k] = sighting{mtime: mtime, lastChange: now}
}

// Stalled reports whether no change to k has been observed for StallAfter at observer time now.
// A ticket never observed is not stalled: its timer has not started.
func (o *Observer) Stalled(k Key, now time.Duration) bool {
	s, ok := o.seen[k]
	return ok && now-s.lastChange >= StallAfter
}

// Forget drops k, for a ticket that is gone.
func (o *Observer) Forget(k Key) {
	delete(o.seen, k)
}
