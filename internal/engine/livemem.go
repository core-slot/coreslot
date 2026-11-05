//go:build unix

package engine

import (
	"cmp"
	"context"
	"path/filepath"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// liveHold is one live hold as admission and the watch tick read it.
type liveHold struct {
	name string
	body wire.Hold
}

// holdListing lists holds/ for admission, the watch tick and Snapshot alike: the live holds, a
// lock probe each, the dead ones awaiting reconciliation, and how many entries were stray or did
// not match the grammar. A live hold whose body does not parse (its run rewrites it in place, so
// a read can be torn) reserves coldStartMiB, with the pid and start its name carries: unknown is
// not zero (PROTOCOL.md section 7.2). It fails when holds/ or a body cannot be read.
func holdListing(st *store.Store, coldStartMiB int64) (live []liveHold, dead []string, ignored int, err error) {
	entries, ignored, err := st.List(store.HoldsDir)
	if err != nil {
		return nil, nil, 0, err
	}
	for _, en := range entries {
		body, err := st.ReadSmall(filepath.Join(store.HoldsDir, en.Name), wire.MaxBodyBytes)
		if gone(err) {
			continue
		}
		if err == nil {
			var isDead bool
			if isDead, err = probe(st, store.HoldsDir, en.Name); err == nil && isDead {
				dead = append(dead, en.Name)
				continue
			}
		}
		switch {
		case gone(err):
			continue
		case stray(err):
			ignored++
			continue
		case err != nil:
			return nil, nil, 0, err
		}
		h, err := wire.DecodeHold(body)
		if err != nil || uint64(h.StartNS) != en.ID.OrderKey || h.PID != en.ID.PID {
			h = wire.Hold{PID: en.ID.PID, StartNS: int64(en.ID.OrderKey), NeedMiB: coldStartMiB}
		}
		live = append(live, liveHold{name: en.Name, body: h})
	}
	return live, dead, ignored, nil
}

// info is the hold as a waiter or a snapshot shows it.
func (h liveHold) info() HolderInfo {
	b := h.body
	return HolderInfo{Name: h.name, Label: b.Label, PID: b.PID, StartNS: b.StartNS, ArrivalNS: b.ArrivalNS,
		NeedMiB: b.NeedMiB, FootprintMiB: b.FootprintMiB, Stops: b.Stops}
}

// reservations are the room's input: each live hold that reserves memory, with the footprint its
// run last wrote.
func reservations(holds []liveHold) []state.MemHold {
	out := make([]state.MemHold, 0, len(holds))
	for _, h := range holds {
		if h.body.NeedMiB > 0 {
			out = append(out, state.MemHold{NeedMiB: h.body.NeedMiB, FootprintMiB: h.body.FootprintMiB})
		}
	}
	return out
}

// Tick is the grant's part in the shared watch tick: inside the section, entered within
// sectionWait, the detector advances on a new machine sample of the host and the live holds when
// the stored one is 1.5 s old (store.Section.Tick). It returns the detector; a section that
// stayed busy returns the zero detector.
func (g *Grant) Tick(ctx context.Context) (watch.Detector, error) {
	e := g.e
	sec, entered, err := e.enter(ctx, false)
	if err != nil || !entered {
		return watch.Detector{}, err
	}
	defer sec.Leave()
	if n, understood, err := sec.Format(); err != nil || !understood {
		return watch.Detector{}, cmp.Or(err, refuseFormat(n, ""))
	}
	cfg, m, err := e.reading()
	if err != nil {
		return watch.Detector{}, err
	}
	d, _, err := sec.Tick(e.clock.Realtime(), e.sampler(cfg, m), cfg.Watch)
	return d, err
}

// headroom is the headroom the room comes from (PROTOCOL.md section 5.1), and its mode: on macOS
// the reading of m's parts in the mode that rate, the compression rate of the last period (nil
// when unknown or stale), and the live holds choose, with their footprints left out of the
// anonymous memory; elsewhere, and under the test hook, m's headroom as the kernel gave it.
func headroom(m config.Machine, holds []liveHold, rate *float64) (mib int64, mode string) {
	if m.HeadroomParts == nil {
		return m.HeadroomMiB, ""
	}
	return m.HeadroomParts.Headroom(footprints(holds), rate, len(holds) > 0)
}

// shortHeadroom is the headroom a stop round's shortage comes from (PROTOCOL.md section 23.5): on
// macOS always the cheap reading, whatever the compression rate; elsewhere m's headroom.
func shortHeadroom(m config.Machine, holds []liveHold) int64 {
	if m.HeadroomParts == nil {
		return m.HeadroomMiB
	}
	return m.HeadroomParts.Cheap(footprints(holds))
}

// footprints is the sum of every live hold's footprint, in MiB, a run outside the budget's too.
func footprints(holds []liveHold) (mib int64) {
	for _, h := range holds {
		mib += h.body.FootprintMiB
	}
	return mib
}

// readHost reads the host for a machine sample; tests replace it with fixed counters.
var readHost = memsample.ReadHost

// storedRate is the compression rate of the stored sample s, taken at at, while the sample is
// fresh at now (both by the wall clock); nil otherwise.
func storedRate(s watch.Sample, at, now int64) *float64 {
	if !fresh(at, now) {
		return nil
	}
	return s.CompressBytesS
}

// sampler reads one machine sample, given the stored one: the host, the running jobs, the
// compression rate since the stored sample (macOS), the room from the headroom in the mode that
// rate chooses, and the shortage from the cheap one.
func (e *Engine) sampler(cfg config.Config, m config.Machine) func(watch.Sample) (watch.Sample, error) {
	return func(prev watch.Sample) (watch.Sample, error) {
		host, err := readHost()
		if err != nil {
			return watch.Sample{}, err
		}
		holds, _, _, err := holdListing(e.st, spec.ColdStartMiB(m.MemMiB))
		if err != nil {
			return watch.Sample{}, err
		}
		s := watch.Sample{Host: host, Jobs: runningJobs(holds, e.clock.Realtime())}
		for _, h := range holds {
			s.Live = append(s.Live, h.name)
		}
		if m.HeadroomOK {
			if r, ok := memsample.CompressRate(prev.Host, host); ok && m.HeadroomParts != nil {
				s.CompressBytesS = &r
			}
			mib, mode := headroom(m, holds, s.CompressBytesS)
			short := shortHeadroom(m, holds)
			s.RoomMiB = state.Room(mib, cfg.ReserveMiB, reservations(holds))
			s.ShortMiB = max(cfg.ReserveMiB-short, 0)
			s.HeadroomMiB, s.HeadroomParts, s.HeadroomMode = &mib, m.HeadroomParts, mode
			if m.HeadroomParts != nil {
				s.HeadroomCheapMiB = &short
			}
		}
		return s, nil
	}
}

// runningJobs are the watch jobs of holds at nowNS, each with the normal share its own run wrote:
// the holds whose command is alive and whose run ticked within memsample.StaleNS. Any other hold
// has no time to report.
func runningJobs(holds []liveHold, nowNS int64) []watch.Job {
	var jobs []watch.Job
	for _, h := range holds {
		b := h.body
		if b.ChildExited || !fresh(b.SampledNS, nowNS) {
			continue
		}
		jobs = append(jobs, watch.Job{Hold: h.name, Label: b.Label, ArrivalNS: b.ArrivalNS,
			NeedMiB: b.NeedMiB, FootprintMiB: b.FootprintMiB, DUserNS: b.DUserNS, DSysNS: b.DSysNS,
			DPageWaitNS: b.DPageWaitNS, NormalShare: b.NormalShare, Learned: b.Learned, Victim: b.Victim,
			CPUNS: b.CPUNS, TypicalCPUS: b.TypicalCPUS, TypicalS: b.TypicalS, StallEWMA: b.StallEWMA})
	}
	return jobs
}
