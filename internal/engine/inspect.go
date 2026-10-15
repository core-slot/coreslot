//go:build unix

package engine

import (
	"fmt"
	"maps"
	"slices"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// Free returns resource's size minus its locked tokens now (PROTOCOL.md section 12.1 step 4),
// never below 0. It counts by probing every existing token file outside the section
// (store.CountLockedProbe), which the protocol permits, so it never waits for the section and
// works under any format. A
// resource the configuration does not declare is an error matching ErrRefused.
func (e *Engine) Free(resource string) (int, error) {
	if err := spec.ValidateName(resource); err != nil {
		return 0, err
	}
	sizes, err := e.sizes()
	if err != nil {
		return 0, err
	}
	size, ok := sizes[resource]
	if !ok {
		return 0, fmt.Errorf("%w: resource %q is not declared in the configuration", ErrRefused, resource)
	}
	locked, err := e.st.CountLockedProbe(resource)
	if err != nil {
		return 0, err
	}
	return max(0, size-locked), nil
}

// Snapshot reads the state directory outside the section, so that it works while another
// client holds (or is stopped inside) the section, and writes nothing: it creates no file, not
// even admit.lock, and works on a store opened read-only over a directory that does not exist
// (StateDirExists is then false and everything else empty). Only a state error that makes the
// directory unreadable is returned; a malformed FORMAT, an invalid configuration and malformed
// or stray entries are reported in the Report.
func (e *Engine) Snapshot() (Report, error) {
	r := Report{StateDir: e.st.Dir(), StateDirExists: e.st.Exists(), Resources: map[string]ResourceInfo{},
		UndeclaredLocked: map[string]int{}, Ignored: map[string]int{}}

	n, present, err := e.st.ReadFormat()
	// Format 0 is well-formed and names a format this document does not define (PROTOCOL.md
	// section 7.6), so Format alone cannot tell it from an absent FORMAT: FormatPresent does.
	r.FormatPresent = present
	switch {
	case err != nil:
		r.FormatError, r.FormatErr = err.Error(), err
	case !present:
		r.FormatUnderstood = true // the first entry writes format 1
	default:
		r.Format, r.FormatUnderstood = n, store.Understood(n)
	}
	g, err := e.st.ReadGen()
	if err != nil {
		return Report{}, err
	}
	r.Gen, r.ReconciledNS = g.N, g.ReconciledNS

	if sizes, err := e.sizes(); err != nil {
		r.ConfigError, r.ConfigErr = reasonOf(err), err
	} else {
		for _, name := range slices.Sorted(maps.Keys(sizes)) {
			locked, err := e.st.CountLockedProbe(name)
			if err != nil {
				return Report{}, err
			}
			r.Resources[name] = ResourceInfo{Size: sizes[name], Locked: locked, Available: max(0, sizes[name]-locked)}
		}
		// Tokens of a resource the configuration no longer declares are still locks (PROTOCOL.md
		// section 6): report them.
		dirs, err := e.st.Resources()
		if err != nil {
			return Report{}, err
		}
		for _, name := range dirs {
			if _, declared := sizes[name]; declared {
				continue
			}
			locked, err := e.st.CountLockedProbe(name)
			if err != nil {
				return Report{}, err
			}
			if locked > 0 {
				r.UndeclaredLocked[name] = locked
			}
		}
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
			Priority: tk.Priority, PID: en.ID.PID, Need: maps.Clone(tk.Need), CreatedNS: tk.CreatedNS,
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
