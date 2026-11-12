//go:build unix

package engine

import (
	"cmp"
	"slices"

	"github.com/core-slot/coreslot/internal/watch"
)

// headWait is the expected wait W of PROTOCOL.md section 10.3 for the head of the queue, as the
// passing rule judges it: 0 with no live hold (the idle rule); unknown with the headroom unread;
// otherwise the live holds with a typical run time end soonest first, each freeing the larger of
// its need and its footprint, until room covers needMiB, and a restart head waits at least the
// calm it still needs. noPass is "headroom_unknown", "wait_unknown" or "watch_unreadable" when W
// is unknown, and w is then 0. readWatch is called only for a restart head.
func headWait(needMiB int64, restart bool, holds []liveHold, room, nowNS int64, headroomOK bool,
	readWatch func() (watch.Detector, watch.Sample, int64, error), c watch.Config) (w int64, noPass string) {
	switch {
	case len(holds) == 0:
		return 0, ""
	case !headroomOK:
		return 0, "headroom_unknown"
	}
	known := slices.DeleteFunc(slices.Clone(holds), func(h liveHold) bool { return h.body.TypicalS == 0 })
	slices.SortFunc(known, func(a, b liveHold) int {
		return cmp.Compare(holdLeftS(a.body, nowNS), holdLeftS(b.body, nowNS))
	})
	for _, h := range known {
		if room >= needMiB {
			break
		}
		room, w = room+max(h.body.NeedMiB, h.body.FootprintMiB), holdLeftS(h.body, nowNS)
	}
	if room < needMiB {
		return 0, "wait_unknown"
	}
	if restart {
		d, s, at, err := readWatch()
		if err != nil {
			return 0, "watch_unreadable"
		}
		w = max(w, calmLeft(d, s, at, nowNS, c)/1e9)
	}
	return w, ""
}
