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

// ReadWatch reads the shared watch state: the detector, the machine sample it last advanced on and
// when, by NowNS, that sample was taken. A missing or malformed watch.json is the zero state. It
// needs no section: watch.json is replaced whole, never rewritten.
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

// Tick advances the detector on a new sample only when the stored one is 1.5 s old (now is NowNS),
// so no early ticker leaves the next one 4 s away. read takes the new sample, given the stored
// one. A failed read is stored as Unknown and returned.
func (sec *Section) Tick(now int64, read func(prev watch.Sample) (watch.Sample, error), c watch.Config) (watch.Detector, []string, error) {
	d, prev, at, err := sec.s.ReadWatch()
	if err != nil || at <= now && now-at < memsample.PeriodNS-memsample.PeriodNS/4 { // this period's sample
		return d, d.Stop, err
	}
	cur, rerr := read(prev)
	if rerr != nil {
		cur = watch.Sample{} // no reading: Unknown, and the next sample starts afresh
	}
	if rerr == nil { // a listing that was read says which holds are gone
		d.Kept = watch.Prune(d.Kept, cur.Live)
	}
	var round []string
	if lvl := d.Advance(prev, cur, c); lvl >= watch.Red && roundOver(d, cur, c) { // red or floor: a stop round
		round, d.Kept = watch.StopList(cur.Jobs, lvl == watch.Floor, cur.ShortMiB, d.Kept, cur.Host.WallNS)
		d.Stop, d.LastRoundNS, d.Observed, d.RoundFloor = round, cur.Host.WallNS, c.Observe, lvl == watch.Floor
	}
	if c.SampleLog { // a calibration aid: a failed rotation or write costs a line
		if sec.rotateLogIfNeeded(WatchLog, WatchLogOld) == nil {
			_ = sec.s.appendSample(cur, d, round)
		}
	}
	if err := sec.SetWatch(d, cur, now); err != nil {
		return d, d.Stop, err
	}
	return d, d.Stop, rerr
}

// roundOver reports whether a new stop round may start: post_kill_s has passed since the last
// one, and every hold it listed is gone from the running jobs. A round observe mode wrote,
// whose holds never exit, holds back no round under enforce.
func roundOver(d watch.Detector, cur watch.Sample, c watch.Config) bool {
	if d.Observed && !c.Observe {
		return true
	}
	if since := cur.Host.WallNS - d.LastRoundNS; since >= 0 && since < int64(c.PostKillS*1e9) {
		return false
	}
	return !slices.ContainsFunc(cur.Jobs, func(j watch.Job) bool { return slices.Contains(d.Stop, j.Hold) })
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
	if cur.HeadroomMiB != nil {
		l["headroom_mib"] = *cur.HeadroomMiB
	}
	if p := cur.HeadroomParts; p != nil {
		l["headroom_free_mib"], l["headroom_speculative_mib"], l["headroom_purgeable_mib"], l["headroom_file_backed_mib"] =
			p.FreeMiB, p.SpeculativeMiB, p.PurgeableMiB, p.FileBackedMiB
		l["headroom_internal_mib"] = p.InternalMiB
	}
	if cur.HeadroomCheapMiB != nil {
		l["headroom_cheap_mib"] = *cur.HeadroomCheapMiB
	}
	if cur.HeadroomMode != "" {
		l["headroom_mode"] = cur.HeadroomMode
	}
	if cur.CompressBytesS != nil {
		l["compress_bytes_s"] = *cur.CompressBytesS
	}
	line, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return s.appendLine(WatchLog, append(line, '\n'))
}
