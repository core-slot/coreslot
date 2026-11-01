//go:build unix

package engine

import (
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
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
		r.Tickets = append(r.Tickets, TicketInfo{Name: en.Name, ID: en.ID.String(), Label: tk.Label,
			PID: en.ID.PID, NeedMiB: tk.NeedMiB, CreatedNS: tk.CreatedNS,
			MtimeNS: en.MtimeNS, Dead: dead})
	}

	holds, ignored, err := e.st.List(store.HoldsDir)
	if err != nil {
		return Report{}, err
	}
	r.Ignored[store.HoldsDir] = ignored
	for _, en := range holds {
		body, err := e.readBody(store.HoldsDir, en.Name)
		if gone(err) {
			continue
		}
		if stray(err) {
			r.Ignored[store.HoldsDir]++
			continue
		}
		if err != nil {
			return Report{}, err
		}
		info := holderInfo(en, body)
		if info == nil {
			r.Ignored[store.HoldsDir]++
			continue
		}
		dead, err := e.st.Probe(store.HoldsDir, en.Name)
		if gone(err) {
			continue
		}
		if stray(err) {
			r.Ignored[store.HoldsDir]++
			continue
		}
		if err != nil {
			return Report{}, err
		}
		if dead {
			r.DeadHolds = append(r.DeadHolds, en.Name)
		} else {
			r.Holds = append(r.Holds, *info)
		}
	}
	if memErr == nil && machine.HeadroomOK {
		held := make([]state.MemHold, 0, len(r.Holds))
		for _, h := range r.Holds {
			r.Mem.ReservedMiB += h.NeedMiB
			if h.NeedMiB > 0 {
				held = append(held, state.MemHold{NeedMiB: h.NeedMiB, FootprintMiB: h.FootprintMiB})
			}
		}
		room := state.Room(machine.HeadroomMiB, cfg.ReserveMiB, held)
		r.Mem.Known, r.Mem.SizeMiB, r.Mem.FreeMiB = true, r.Mem.ReservedMiB+room, max(0, room)
	}
	// A live hold's lock outlives its owner only in a descendant that inherited the hold's
	// descriptor. An unreadable process table reports nothing: unknown is not exited.
	if procs, ok := listProcs(); ok {
		for i, h := range r.Holds {
			r.Holds[i].OwnerExited = !memsample.Alive(procs, h.PID, h.StartNS)
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
