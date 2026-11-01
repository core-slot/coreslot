//go:build unix

package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"time"

	"github.com/core-slot/coreslot/internal/watch"
)

const (
	// tickNS is the sampling period: a stored sample younger than it is this period's.
	tickNS = int64(2 * time.Second)
	// maxWatchBytes bounds watch.json, which grows with the live holds.
	maxWatchBytes = 1 << 20
)

// watchState is the body of watch.json.
type watchState struct {
	SampledNS int64          `json:"sampled_ns"`
	Sample    watch.Sample   `json:"sample"`
	Detector  watch.Detector `json:"detector"`
}

// Watch reads the shared watch state: the detector, the machine sample it last advanced on and
// when, by NowNS, that sample was taken. A missing or malformed watch.json is the zero state.
func (sec *Section) Watch() (watch.Detector, watch.Sample, int64, error) {
	var w watchState
	body, err := sec.s.ReadSmall(WatchFile, maxWatchBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return w.Detector, w.Sample, 0, err
	case json.Unmarshal(body, &w) != nil: // a body over the bound is cut, so malformed too
		w = watchState{}
	}
	return w.Detector, w.Sample, w.SampledNS, nil
}

// SetWatch replaces the shared watch state.
func (sec *Section) SetWatch(d watch.Detector, s watch.Sample, sampledNS int64) error {
	body, err := json.Marshal(watchState{SampledNS: sampledNS, Sample: s, Detector: d})
	if err != nil {
		return err
	}
	return sec.replaceFile(WatchFile, body)
}

// Tick advances the detector on a new sample only when the stored one is 2 s old (now is NowNS),
// so one sample serves the machine per period. A failed read is stored as Unknown and returned.
func (sec *Section) Tick(now int64, read func() (watch.Sample, error), c watch.Config) (watch.Detector, []string, error) {
	d, prev, at, err := sec.Watch()
	if err != nil || at <= now && now-at < tickNS {
		return d, d.Stop, err
	}
	cur, rerr := read()
	if rerr != nil {
		cur = watch.Sample{} // no reading: Unknown, and the next sample starts afresh
	}
	if d.Advance(prev, cur, c) >= watch.Red && roundOver(d, cur, c) { // red or floor: a stop round
		d.Stop, d.LastRoundNS = watch.StopList(cur.Jobs, max(-cur.RoomMiB, 0)), cur.Host.WallNS
	}
	if err := sec.SetWatch(d, cur, now); err != nil {
		return d, d.Stop, err
	}
	return d, d.Stop, rerr
}

// roundOver reports whether a new stop round may start: post_kill_s has passed since the last
// one, and every hold it listed is gone or reports its child exited.
func roundOver(d watch.Detector, cur watch.Sample, c watch.Config) bool {
	if since := cur.Host.WallNS - d.LastRoundNS; since >= 0 && since < int64(c.PostKillS*1e9) {
		return false
	}
	return !slices.ContainsFunc(cur.Jobs, func(j watch.Job) bool {
		return !j.ChildExited && slices.Contains(d.Stop, j.Hold)
	})
}
