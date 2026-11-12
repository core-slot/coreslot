//go:build unix

package engine

import (
	"time"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// listProcs reads the process table for Snapshot's owner check; tests replace it.
var listProcs = memsample.Listing

// Snapshot reads the state directory outside the section, so that it works while another
// client holds (or is stopped inside) the section, and writes nothing: it creates no file, not
// even admit.lock, and works on a store opened read-only over a directory that does not exist
// (StateDirExists is then false and everything else empty). Only a state error that makes the
// directory unreadable is returned; a malformed FORMAT, an invalid configuration and malformed
// or stray entries are reported in the Report.
func (e *Engine) Snapshot() (Report, error) {
	r := Report{StateDir: e.st.Dir(), StateDirExists: e.st.Exists(), Ignored: map[string]int{}}

	n, present, err := e.st.ReadFormat()
	// Format 0 is well-formed and names a format this document does not define (PROTOCOL.md
	// section 7.6), so Format alone cannot tell it from an absent FORMAT: FormatPresent does.
	r.FormatPresent = present
	switch {
	case err != nil:
		r.FormatError, r.FormatErr = err.Error(), err
	case !present:
		r.FormatUnderstood = true // the first entry writes format 2
	default:
		r.Format, r.FormatUnderstood = n, store.Understood(n)
	}
	g, err := e.st.ReadGen()
	if err != nil {
		return Report{}, err
	}
	r.Gen, r.ReconciledNS = g.N, g.ReconciledNS

	cfg, machine, memErr := e.reading()
	if memErr != nil {
		r.ConfigError, r.ConfigErr = reasonOf(memErr), memErr
	}

	tickets, ignored, err := e.st.List(store.QueueDir)
	if err != nil {
		return Report{}, err
	}
	r.Ignored[store.QueueDir] = ignored
	// The first live ticket in queue order is the head the passing rule judges others by: only its
	// index, need and restart are kept for its expected wait.
	head, headNeed, headRestart := -1, int64(0), false
	for _, en := range tickets {
		body, err := e.readBody(store.QueueDir, en.Name)
		if gone(err) {
			continue
		}
		if stray(err) {
			r.Ignored[store.QueueDir]++
			continue
		}
		if err != nil {
			return Report{}, err
		}
		tk, err := wire.DecodeTicket(body)
		if err != nil {
			r.Ignored[store.QueueDir]++
			continue
		}
		dead, err := e.st.Probe(store.QueueDir, en.Name)
		if gone(err) {
			continue
		}
		if stray(err) {
			r.Ignored[store.QueueDir]++
			continue
		}
		if err != nil {
			return Report{}, err
		}
		if !dead && head < 0 {
			head, headNeed, headRestart = len(r.Tickets), tk.NeedMiB, tk.Restart
		}
		r.Tickets = append(r.Tickets, TicketInfo{Name: en.Name, ID: en.ID.String(), Label: tk.Label,
			PID: en.ID.PID, NeedMiB: tk.NeedMiB, CreatedNS: tk.CreatedNS,
			MtimeNS: en.MtimeNS, Dead: dead})
	}

	live, dead, ignored, err := holdListing(e.st, spec.ColdStartMiB(machine.MemMiB))
	if err != nil {
		return Report{}, err
	}
	r.Ignored[store.HoldsDir], r.DeadHolds = ignored, dead
	listed := e.clock.Realtime()
	for _, h := range live {
		info := h.info()
		info.ChildExited, info.SampleAgeMS = h.body.ChildExited, sampleAgeMS(h.body.SampledNS, listed)
		info.LeftS = holdLeftS(h.body, listed)
		r.Holds = append(r.Holds, info)
		r.Mem.ReservedMiB += h.body.NeedMiB
	}
	if memErr == nil {
		r.Mem.HeadroomMiB, r.Mem.HeadroomOK, r.Mem.ReserveMiB = machine.HeadroomMiB, machine.HeadroomOK, cfg.ReserveMiB
	}
	var room int64 // 0 while the headroom is unread
	if memErr == nil && machine.HeadroomOK {
		r.Mem.Known = true
		room = state.Room(machine.HeadroomMiB, cfg.ReserveMiB, reservations(live))
		r.Mem.RoomMiB, r.Mem.FreeMiB, r.Mem.MachineMiB = room, max(0, room), machine.MemMiB
		r.Mem.RealFreeMiB = max(0, state.Room(machine.HeadroomMiB, cfg.ReserveMiB, nil))
	}
	// A live hold's lock outlives its owner only in a descendant that inherited the hold's
	// descriptor. An unreadable process table reports nothing: unknown is not exited.
	if procs, ok := listProcs(); ok {
		for i, h := range r.Holds {
			r.Holds[i].OwnerExited = !memsample.Alive(procs, h.PID, h.StartNS)
			r.Holds[i].Stoppable = live[i].body.Victim && !r.Holds[i].OwnerExited && !h.ChildExited
		}
	}

	d, smp, at, watchErr := e.st.ReadWatch() // no readable watch.json is no sample
	if head >= 0 && memErr == nil {
		readWatch := func() (watch.Detector, watch.Sample, int64, error) { return d, smp, at, watchErr }
		w, noPass := headWait(headNeed, headRestart, live, room, listed, machine.HeadroomOK, readWatch, cfg.Watch)
		r.Tickets[head].WaitS, r.Tickets[head].WaitKnown = w, noPass == ""
	}
	now := e.clock.Realtime()
	r.Watch = WatchInfo{Level: d.Level, Excess: d.Excess, SampledNS: at, Observe: cfg.Watch.Observe,
		Fresh: fresh(at, now)}
	if r.Watch.Fresh { // the newest sample knows which jobs have a learned normal S; this client does not
		if next, _ := watch.StopList(smp.Jobs, d.Level == watch.Floor, 1, d.Kept, smp.Host.WallNS+now-at); len(next) > 0 {
			r.Watch.NextVictim = next[0]
		}
		for _, j := range smp.Jobs {
			if !j.Learned {
				r.Watch.Learning++
			}
		}
	}

	cancels, ignored, err := e.st.List(store.CancelDir)
	if err != nil {
		return Report{}, err
	}
	r.Ignored[store.CancelDir] = ignored
	for _, en := range cancels {
		r.Cancels = append(r.Cancels, en.Name)
	}

	r.Section, err = e.sectionInfo()
	if err != nil {
		return Report{}, err
	}
	rec, err := e.st.Degraded()
	if err != nil {
		return Report{}, err
	}
	if rec != nil {
		r.Degraded = &DegradedInfo{Reason: rec.Reason, FirstNS: rec.FirstNS, Count: rec.Count, Malformed: rec.Malformed}
	}
	return r, nil
}

// sampleAgeMS is the age at nowNS of a hold's sample taken at sampledNS, -1 when there was none.
func sampleAgeMS(sampledNS, nowNS int64) int64 {
	if sampledNS == 0 {
		return -1
	}
	return max(0, nowNS-sampledNS) / int64(time.Millisecond)
}

// sectionInfo probes admit.lock (one single-attempt flock, released at once, never creating the
// file) and reads its informational body, tolerating an empty, stale or malformed one
// (PROTOCOL.md section 7.7).
func (e *Engine) sectionInfo() (SectionInfo, error) {
	var info SectionInfo
	held, err := e.st.SectionHeld()
	if err != nil {
		return info, err
	}
	info.Held = held
	if a, ok := e.sectionHolder(); ok {
		info.PID, info.EnteredNS = a.PID, a.EnteredNS
	}
	return info, nil
}

// sectionHolder reads admit.lock's informational body (PROTOCOL.md section 7.7); ok is false when
// it is missing, unreadable or malformed. It is for display only.
func (e *Engine) sectionHolder() (wire.AdmitLock, bool) {
	body, err := e.readBody("", store.AdmitLock)
	if err != nil {
		return wire.AdmitLock{}, false
	}
	a, err := wire.DecodeAdmitLock(body)
	return a, err == nil
}
