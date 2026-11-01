// Package state is the pure state machine of the protocol: election, observed freshness and the
// room the live holds leave. It performs no I/O and reads no clock; time is always a parameter.
package state

import (
	"cmp"
	"time"
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
	Dead bool
}

// PassedOver reports whether the election (PROTOCOL.md section 10.3) skips t: it is dead or, for
// obs at now, STALLED. The election scans the tickets ahead from the nearest, and the first one
// that is not passed over ends it.
func PassedOver(t Ticket, obs *Observer, now time.Duration) bool {
	return t.Dead || obs.Stalled(t.Key, now)
}
