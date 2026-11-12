//go:build unix

package engine

import (
	"errors"
	"testing"

	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// headWait is the expected wait of the head of the queue. Mutations: dropping the no-hold branch
// reddens "no hold, room short"; sorting by typical_s instead of by time left reddens "out of
// order"; returning the partial W beside an unknown one reddens "partial cover".
func TestHeadWait(t *testing.T) {
	const now = int64(1_000_000) * 1e9
	started := func(agoS int64) int64 { return now - agoS*1e9 }
	mk := func(typical, agoS, need int64) liveHold {
		return liveHold{body: wire.Hold{TypicalS: typical, StartNS: started(agoS), NeedMiB: need}}
	}
	unread := func() (watch.Detector, watch.Sample, int64, error) {
		t.Error("the watch was read for a head that is no restart")
		return watch.Detector{}, watch.Sample{}, 0, errors.New("unread")
	}
	for _, c := range []struct {
		name     string
		need     int64
		holds    []liveHold
		room     int64
		headroom bool
		wantW    int64
		wantNo   string
	}{
		{"no hold, room short", 2000, nil, 0, true, 0, ""},
		{"headroom unread, a hold live", 2000, []liveHold{mk(100, 0, 1000)}, 0, false, 0, "headroom_unknown"},
		// A is the sooner to end (20 s left against 50 s) though B has the shorter typical time.
		{"out of order", 1500, []liveHold{mk(50, 0, 1000), mk(100, 80, 1000)}, 500, true, 20, ""},
		{"in order", 1500, []liveHold{mk(100, 80, 1000), mk(50, 0, 1000)}, 500, true, 20, ""},
		{"outlived hold covers", 1000, []liveHold{mk(60, 600, 1000)}, 0, true, 0, ""},
		{"needed hold without a typical time", 1000, []liveHold{mk(0, 0, 1000)}, 0, true, 0, "wait_unknown"},
		// The hold with a typical time frees 500 at 100 s; the other, without one, is never counted.
		{"partial cover", 2000, []liveHold{mk(100, 0, 500), mk(0, 0, 1000)}, 0, true, 0, "wait_unknown"},
	} {
		w, no := headWait(c.need, false, c.holds, c.room, now, c.headroom, unread, watch.Config{})
		if w != c.wantW || no != c.wantNo {
			t.Errorf("%s: (%d, %q), want (%d, %q)", c.name, w, no, c.wantW, c.wantNo)
		}
	}
}
