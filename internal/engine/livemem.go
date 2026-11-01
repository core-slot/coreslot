//go:build unix

package engine

import (
	"cmp"
	"context"
	"path/filepath"

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

// liveHolds lists holds/ and returns the live holds, a lock probe each, as Ticket.Holders does;
// stray holds are left out. A live hold whose body does not parse (its run rewrites it in place,
// so a read can be torn) reserves coldStartMiB: unknown is not zero. It fails when holds/ or a
// body cannot be read. It takes no lock of its own: admit runs under stepMu, which
// Ticket.Holders takes.
func liveHolds(st *store.Store, coldStartMiB int64) ([]liveHold, error) {
	entries, _, err := st.List(store.HoldsDir)
	if err != nil {
		return nil, err
	}
	var live []liveHold
	for _, en := range entries {
		body, err := st.ReadSmall(filepath.Join(store.HoldsDir, en.Name), wire.MaxBodyBytes)
		if gone(err) || stray(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		h, ok := decodeHold(en, body)
		dead, err := probe(st, store.HoldsDir, en.Name)
		if gone(err) || stray(err) || (err == nil && dead) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			h = wire.Hold{NeedMiB: coldStartMiB}
		}
		live = append(live, liveHold{name: en.Name, body: h})
	}
	return live, nil
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
// the stored one is a period old (store.Section.Tick). It returns the detector and the [watch]
// configuration; a section that stayed busy returns the zero detector. normal holds the labels'
// normal shares; a label it lacks has watch.DefaultNormalShare.
func (g *Grant) Tick(ctx context.Context, normal map[string]float64) (watch.Detector, watch.Config, error) {
	e := g.e
	sec, entered, err := e.enter(ctx, false)
	if err != nil || !entered {
		return watch.Detector{}, watch.Config{}, err
	}
	defer sec.Leave()
	if n, understood, err := sec.Format(); err != nil || !understood {
		return watch.Detector{}, watch.Config{}, cmp.Or(err, refuseFormat(n, ""))
	}
	cfg, m, err := e.reading()
	if err != nil {
		return watch.Detector{}, watch.Config{}, err
	}
	d, _, err := sec.Tick(e.clock.Realtime(), func() (watch.Sample, error) {
		host, err := memsample.ReadHost()
		if err != nil {
			return watch.Sample{}, err
		}
		holds, err := liveHolds(e.st, spec.ColdStartMiB(m.MemMiB))
		if err != nil {
			return watch.Sample{}, err
		}
		s := watch.Sample{Host: host}
		if m.HeadroomOK {
			s.RoomMiB = state.Room(m.HeadroomMiB, cfg.ReserveMiB, reservations(holds))
		}
		for _, h := range holds {
			b := h.body
			share, ok := normal[b.Label]
			if !ok {
				share = watch.DefaultNormalShare
			}
			s.Jobs = append(s.Jobs, watch.Job{Hold: h.name, Label: b.Label, ArrivalNS: b.ArrivalNS,
				NeedMiB: b.NeedMiB, FootprintMiB: b.FootprintMiB, DUserNS: b.DUserNS, DSysNS: b.DSysNS,
				DPageWaitNS: b.DPageWaitNS, NormalShare: share, Victim: b.Victim, ChildExited: b.ChildExited})
		}
		return s, nil
	}, cfg.Watch)
	return d, cfg.Watch, err
}
