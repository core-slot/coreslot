//go:build unix

package engine

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// Free returns resource's size minus its locked tokens now (PROTOCOL.md section 12.1 step 4),
// never below 0. It counts by probing every existing token file outside the section
// (store.CountLockedProbe), which the protocol permits, so it never waits for the section and
// works under any format. A
// resource the configuration does not declare is an error.
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
		return 0, fmt.Errorf("engine: resource %q is not declared in the configuration", resource)
	}
	locked, err := e.st.CountLockedProbe(resource)
	if err != nil {
		return 0, err
	}
	return max(0, size-locked), nil
}

// Snapshot reads the state directory outside the section, so that it works while another
// client holds (or is stopped inside) the section, and writes nothing. Only a state error
// that makes the directory unreadable is returned; a malformed FORMAT, an invalid
// configuration and malformed or stray entries are reported in the Report.
func (e *Engine) Snapshot() (Report, error) {
	r := Report{StateDir: e.st.Dir(), Resources: map[string]ResourceInfo{}, Ignored: map[string]int{}}

	n, present, err := e.st.ReadFormat()
	switch {
	case err != nil:
		r.FormatError = err.Error()
	case !present:
		r.FormatUnderstood = true // the first entry writes format 1
	default:
		r.Format, r.FormatUnderstood = n, slices.Contains(formats(), n)
	}
	g, err := e.st.ReadGen()
	if err != nil {
		return Report{}, err
	}
	r.Gen, r.ReconciledNS = g.N, g.ReconciledNS

	if sizes, err := e.sizes(); err != nil {
		r.ConfigError = err.Error()
	} else {
		for _, name := range slices.Sorted(maps.Keys(sizes)) {
			locked, err := e.st.CountLockedProbe(name)
			if err != nil {
				return Report{}, err
			}
			r.Resources[name] = ResourceInfo{Size: sizes[name], Locked: locked, Free: max(0, sizes[name]-locked)}
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
		need := make(map[string][2]int, len(tk.Need))
		for name, rg := range tk.Need {
			need[name] = [2]int{rg.Min, rg.Max}
		}
		r.Tickets = append(r.Tickets, TicketInfo{Name: en.Name, ID: en.ID.String(), Label: tk.Label,
			Priority: tk.Priority, PID: en.ID.PID, Need: need, CreatedNS: tk.CreatedNS,
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

// sectionInfo probes admit.lock (one single-attempt entry, left at once) and reads its
// informational body, tolerating an empty, stale or malformed one (PROTOCOL.md section 7.7).
func (e *Engine) sectionInfo() (SectionInfo, error) {
	var info SectionInfo
	sec, entered, err := tryEnterSection(e.st)
	if err != nil {
		return info, err
	}
	if entered {
		sec.Leave()
	}
	info.Held = !entered
	body, err := e.readBody("", store.AdmitLock)
	if err != nil {
		return info, nil
	}
	var b struct {
		PID       int   `json:"pid"`
		EnteredNS int64 `json:"entered_ns"`
	}
	if json.Unmarshal(body, &b) == nil {
		info.PID, info.EnteredNS = b.PID, b.EnteredNS
	}
	return info, nil
}
