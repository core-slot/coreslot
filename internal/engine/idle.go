//go:build unix

package engine

import (
	"time"

	"github.com/core-slot/coreslot/internal/config"
)

// The idle wait (PROTOCOL.md section 12.1, step 4): a head that would start by the idle rule over
// the room waits while memory outside coreslot grows, at most IdleWaitMax.
const (
	idleWindow  = 60 * time.Second  // the headroom readings a head judges the trend over
	idleFallMiB = 1024              // a fall of the headroom within idleWindow above this is growth
	IdleWaitMax = 300 * time.Second // from the head's first attempt over the room with no hold live
)

// idleTrend is a ticket's own headroom readings of the last idleWindow, on its monotonic clock, and
// its idle wait. Nothing else reads it: it lives in the ticket's process alone.
type idleTrend struct {
	readings []idleReading
	held     bool          // an attempt over the room with no hold live came: since is its time
	since    time.Duration // monotonic
	waited   bool          // an attempt waited for it
}

type idleReading struct {
	at  time.Duration
	mib int64
}

// note keeps the attempt's headroom reading and drops those older than idleWindow; an attempt
// with a hold live ends the idle wait, so the next idle one counts its bound afresh.
func (w *idleTrend) note(now time.Duration, m config.Machine, idle bool) {
	i := 0
	for i < len(w.readings) && now-w.readings[i].at > idleWindow {
		i++
	}
	w.readings = w.readings[i:]
	if m.HeadroomOK {
		w.readings = append(w.readings, idleReading{now, m.HeadroomMiB})
	}
	if !idle {
		w.held, w.waited = false, false
	}
}

// idleWait reports whether the head, with no hold live and its need over the room, waits instead
// of starting by the idle rule: while its own headroom readings fell by more than idleFallMiB
// within idleWindow, or the kernel's pressure, when it can be read, is not normal; for at most
// IdleWaitMax from its first such attempt, after which it starts as the idle rule says. When it
// starts after waiting, the outcome carries how long. A convenience with no adversary: its anchor
// is this head's own readings of the kernel's headroom and pressure, which no file moves, and the
// bound keeps the idle start reachable for a need the room never fits.
func (t *Ticket) idleWait(m config.Machine, need, room int64) bool {
	w, now := &t.idle, t.e.clock.Monotonic()
	wait := false
	if m.HeadroomOK && need > room {
		if !w.held {
			w.held, w.since = true, now
		}
		top := m.HeadroomMiB
		for _, r := range w.readings {
			top = max(top, r.mib)
		}
		level, read := KernelPressure()
		wait = now-w.since < IdleWaitMax && (top-m.HeadroomMiB > idleFallMiB || read && level != PressureNormal)
	}
	if w.waited && !wait {
		t.out.IdleWaitedS = (now - w.since).Seconds()
	}
	w.waited = w.waited || wait
	return wait
}
