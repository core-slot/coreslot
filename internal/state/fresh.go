package state

import "time"

// StallAfter is how long a ticket's mtime may stay unchanged, on the observer's own monotonic
// clock, before the observer calls the ticket STALLED (PROTOCOL.md section 10.2).
const StallAfter = 10 * time.Second

// MaxPassGap is the longest interval between two consecutive examination passes of one observer
// that counts as watched (PROTOCOL.md section 10.2): 1.5 times the 2 s at which a waiter must
// poll at the least, so that polling at that interval with scheduling jitter on top is still
// watched, while a longer interval means the observer itself was stopped, suspended or starved,
// and it observed nothing meanwhile.
const MaxPassGap = 3 * time.Second

// sighting is what an observer remembers of one ticket: the last mtime value it read, the
// watched time from which the ticket's unchanged time is counted, and when (in watched time) and
// in which pass it was last examined.
type sighting struct {
	mtime int64
	since time.Duration // watched-since is the unchanged time this observer has watched
	at    time.Duration // watched time of the last examination
	pass  uint64        // the pass of the last examination
}

// Observer judges freshness by watching tickets change, never by comparing an mtime with a
// clock: the mtime is only an opaque value that either differs from the last one or does not.
// The zero value is ready to use. An Observer is not safe for concurrent use.
//
// An observer that examines only some tickets each time (a waiter's election stops at the
// first live, non-STALLED ticket, PROTOCOL.md section 10.3) calls Pass before each examination
// pass. Then only the time between examinations in consecutive passes counts towards a stall:
// when a ticket was left out of one or more passes, the interval up to its next examination was
// not watched and is not counted, though a value that changed meanwhile still restarts the
// timer. A ticket is therefore STALLED only after StallAfter of observer time spent watching it
// without a change, and a stopped ticket stops looking fresh after at most StallAfter of that
// time, counted across interruptions.
//
// Nor does the observer's own absence count: when more than MaxPassGap passes between its
// previous examination (the last Observe or Pass of the previous pass) and the next Pass, the
// observer was stopped, suspended or starved, and that interval adds no watched time to any
// ticket. A ticket that stopped while the observer was away is therefore STALLED only after
// StallAfter of watched time once the observer is back. An observer that never calls Pass
// watches every ticket it observes continuously.
type Observer struct {
	seen      map[Key]sighting
	pass      uint64
	last      time.Duration // observer time of the latest Pass or Observe
	unwatched time.Duration // the sum of the absences: watched time is observer time minus this
}

// Pass starts an examination pass at observer time now.
func (o *Observer) Pass(now time.Duration) {
	if o.pass > 0 && now-o.last > MaxPassGap {
		o.unwatched += now - o.last // the observer's own absence is not watched
	}
	o.pass++
	o.last = max(o.last, now)
}

// watched is the watched time at observer time now.
func (o *Observer) watched(now time.Duration) time.Duration { return now - o.unwatched }

// Observe records that the ticket k currently carries mtime, read at observer time now. A first
// sighting, or a value different from the last one, restarts the ticket's timer; the same value
// leaves it running, except that time since an examination in an earlier pass than the
// previous one, and the observer's own absence, are not counted (see Observer).
func (o *Observer) Observe(k Key, mtime int64, now time.Duration) {
	if o.seen == nil {
		o.seen = make(map[Key]sighting)
	}
	o.last = max(o.last, now)
	w := o.watched(now)
	s, ok := o.seen[k]
	if !ok || s.mtime != mtime {
		o.seen[k] = sighting{mtime: mtime, since: w, at: w, pass: o.pass}
		return
	}
	if o.pass > s.pass+1 {
		s.since += w - s.at // the unwatched interval does not count
	}
	s.at, s.pass = w, o.pass
	o.seen[k] = s
}

// Stalled reports whether no change to k has been observed for StallAfter of watched time at
// observer time now. A ticket never observed is not stalled: its timer has not started.
func (o *Observer) Stalled(k Key, now time.Duration) bool {
	s, ok := o.seen[k]
	return ok && o.watched(now)-s.since >= StallAfter
}

// Seen reports whether k has been observed (and not forgotten since), and the last mtime read.
func (o *Observer) Seen(k Key) (mtime int64, ok bool) {
	s, ok := o.seen[k]
	return s.mtime, ok
}

// Forget drops k, for a ticket that is gone.
func (o *Observer) Forget(k Key) {
	delete(o.seen, k)
}
