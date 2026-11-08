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

// Parts reads the machine's memory by part for a sample line; tests replace it. It is called only
// when the sample log is on.
var Parts = memsample.Parts

// rotateTelemetryAt is the size at which a telemetry file is rotated to its one older generation;
// tests lower it.
var rotateTelemetryAt int64 = 256 << 20

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
// so no early ticker leaves the next one 4 s away. A failed read is stored as Unknown and returned.
func (sec *Section) Tick(now int64, read func() (watch.Sample, error), c watch.Config) (watch.Detector, []string, error) {
	d, prev, at, err := sec.s.ReadWatch()
	if err != nil || at <= now && now-at < memsample.PeriodNS-memsample.PeriodNS/4 { // this period's sample
		return d, d.Stop, err
	}
	cur, rerr := read()
	if rerr != nil {
		cur = watch.Sample{} // no reading: Unknown, and the next sample starts afresh
	}
	if rerr == nil { // a listing that was read says which holds are gone
		d.Kept = watch.Prune(d.Kept, cur.Live)
	}
	var round []string
	var note roundNote
	if lvl := d.Advance(prev, cur, c); lvl >= watch.Red { // red or floor: a stop round, or the reason for none
		if ok, why := roundOver(d, cur, c); !ok {
			note.noRound = why
		} else {
			round, d.Kept = watch.StopList(cur.Jobs, lvl == watch.Floor, cur.ShortMiB, d.Kept, cur.Host.WallNS)
			d.Stop, d.LastRoundNS, d.Observed, d.RoundFloor = round, cur.Host.WallNS, c.Observe, lvl == watch.Floor
			note.started = true
			if len(round) == 0 {
				note.noRound = "nobody_stoppable"
			}
		}
	}
	if c.SampleLog { // a failed rotation or write costs a line
		_ = sec.rotateLogIfNeeded(TelemetryDir, EventsFile, EventsOld, rotateTelemetryAt)
		if sec.rotateLogIfNeeded(TelemetryDir, SamplesFile, SamplesOld, rotateTelemetryAt) == nil {
			_ = sec.s.appendSample(now, cur, d, round, note)
		}
	}
	if err := sec.SetWatch(d, cur, now); err != nil {
		return d, d.Stop, err
	}
	return d, d.Stop, rerr
}

// roundOver reports whether a new stop round may start: post_kill_s has passed since the last
// one, and every hold it listed is gone from the running jobs or no victim any more: a run that
// found a container client since ignores the stop. A round observe mode wrote, whose holds never
// exit, holds back no round under enforce. When it may not, why is post_kill or listed_alive.
func roundOver(d watch.Detector, cur watch.Sample, c watch.Config) (ok bool, why string) {
	if d.Observed && !c.Observe {
		return true, ""
	}
	if since := cur.Host.WallNS - d.LastRoundNS; since >= 0 && since < int64(c.PostKillS*1e9) {
		return false, "post_kill"
	}
	if slices.ContainsFunc(cur.Jobs, func(j watch.Job) bool { return j.Victim && slices.Contains(d.Stop, j.Hold) }) {
		return false, "listed_alive"
	}
	return true, ""
}

// roundNote is what a red or floor sample says of its stop round: that one started, or why none did
// (post_kill, listed_alive, or nobody_stoppable for a round that listed no hold).
type roundNote struct {
	started bool
	noRound string
}

// appendSample appends cur and the detector d that advanced on it to telemetry/samples.jsonl
// (docs/telemetry.md); now is the Tick's and stop is the round written at this sample.
func (s *Store) appendSample(now int64, cur watch.Sample, d watch.Detector, stop []string, note roundNote) error {
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
	l := map[string]any{"v": TelemetrySchema, "time_ns": now, "live_holds": len(cur.Live), "headroom_mib": cur.HeadroomMiB,
		"headroom_ok": cur.HeadroomOK, "reserve_mib": cur.ReserveMiB, "t_ns": h.WallNS, "awake_ns": h.AwakeNS, "level": d.Level.String(), "excess": d.Excess,
		"bucket": d.Bucket, "gate": d.Gate, "decomp_s": d.Rates.DecompS, "swapins_s": d.Rates.SwapInsS,
		"psi_some_us_s": psi, "swap_used_mib": h.SwapUsedMiB, "swap_free_mib": h.SwapFreeMiB,
		"vm_free_mib": h.VMVolumeFreeMiB, "room_mib": cur.RoomMiB, "jobs": jobs}
	if len(stop) > 0 {
		l["stop"] = stop
	}
	if note.noRound != "" {
		l["no_round"] = note.noRound
	}
	if note.started {
		var victims, kept []string
		for _, j := range cur.Jobs {
			if j.Victim {
				victims = append(victims, j.Hold)
			}
		}
		for hold, k := range d.Kept {
			if k.RoundNS == cur.Host.WallNS { // an older round's record is not this round's
				kept = append(kept, hold)
			}
		}
		slices.Sort(kept)
		l["candidates"], l["short_mib"] = victims, cur.ShortMiB
		if len(kept) > 0 {
			l["kept"] = kept
		}
	}
	if parts, pressure, ok := Parts(); ok { // a failed read costs the keys, not the line
		for k, v := range parts {
			l[k] = v
		}
		if pressure != 0 {
			l["pressure_level"] = pressure
		}
	}
	line, err := EncodeJSON(l)
	if err != nil {
		return err
	}
	return AppendFile(s.path(TelemetryDir), SamplesFile, append(line, '\n'))
}
