//go:build unix

package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"slices"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/watch"
)

// maxWatchBytes bounds watch.json, which grows with the live holds.
const maxWatchBytes = 1 << 20

// watchState is the body of watch.json.
type watchState struct {
	SampledNS int64          `json:"sampled_ns"`
	Sample    watch.Sample   `json:"sample"`
	Detector  watch.Detector `json:"detector"`
}

// Watch reads the shared watch state: the detector, the machine sample it last advanced on and
// when, by NowNS, that sample was taken. A missing or malformed watch.json is the zero state.
func (sec *Section) Watch() (watch.Detector, watch.Sample, int64, error) { return sec.s.ReadWatch() }

// ReadWatch is Section.Watch outside the section: watch.json is replaced whole, never rewritten.
func (s *Store) ReadWatch() (watch.Detector, watch.Sample, int64, error) {
	var w watchState
	body, err := s.ReadSmall(WatchFile, maxWatchBytes)
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
	if err != nil || at <= now && now-at < memsample.PeriodNS { // this period's sample
		return d, d.Stop, err
	}
	cur, rerr := read()
	if rerr != nil {
		cur = watch.Sample{} // no reading: Unknown, and the next sample starts afresh
	}
	var round []string
	if d.Advance(prev, cur, c) >= watch.Red && roundOver(d, cur, c) { // red or floor: a stop round
		round = watch.StopList(cur.Jobs, max(-cur.RoomMiB, 0))
		d.Stop, d.LastRoundNS, d.Observed = round, cur.Host.WallNS, c.Observe
	}
	if c.SampleLog {
		_ = sec.s.appendSample(cur, d, round) // a calibration aid: a failed write costs a line
	}
	if err := sec.SetWatch(d, cur, now); err != nil {
		return d, d.Stop, err
	}
	return d, d.Stop, rerr
}

// roundOver reports whether a new stop round may start: post_kill_s has passed since the last
// one, and every hold it listed is gone or reports its child exited. A round observe mode wrote,
// whose holds never exit, holds back no round under enforce.
func roundOver(d watch.Detector, cur watch.Sample, c watch.Config) bool {
	if d.Observed && !c.Observe {
		return true
	}
	if since := cur.Host.WallNS - d.LastRoundNS; since >= 0 && since < int64(c.PostKillS*1e9) {
		return false
	}
	return !slices.ContainsFunc(cur.Jobs, func(j watch.Job) bool {
		return !j.ChildExited && slices.Contains(d.Stop, j.Hold)
	})
}

// appendSample appends cur and the detector d that advanced on it to log/watch.jsonl, as the
// calibration reads it; stop is the round written at this sample.
func (s *Store) appendSample(cur watch.Sample, d watch.Detector, stop []string) error {
	type sampleJob struct {
		watch.Job
		Share *float64 `json:"share"` // null when the job reported no time
	}
	h, jobs := cur.Host, []sampleJob{}
	var psi any // null where the platform has no PSI
	if d.Rates.HasPSI {
		psi = d.Rates.PSISomeUSS
	}
	for _, j := range cur.Jobs {
		sj := sampleJob{Job: j}
		if v, ok := watch.Share(j); ok {
			sj.Share = &v
		}
		jobs = append(jobs, sj)
	}
	l := map[string]any{"t_ns": h.WallNS, "awake_ns": h.AwakeNS, "level": d.Level.String(), "excess": d.Excess,
		"bucket": d.Bucket, "gate": d.Gate, "decomp_s": d.Rates.DecompS, "swapins_s": d.Rates.SwapInsS,
		"psi_some_us_s": psi, "swap_used_mib": h.SwapUsedMiB, "swap_free_mib": h.SwapFreeMiB,
		"vm_free_mib": h.VMVolumeFreeMiB, "room_mib": cur.RoomMiB, "jobs": jobs}
	if len(stop) > 0 {
		l["stop"] = stop
	}
	line, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return s.appendLine(WatchLog, append(line, '\n'))
}
