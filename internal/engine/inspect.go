//go:build unix

package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// Free returns resource's size minus its locked tokens now (PROTOCOL.md section 12.1 step 4),
// never below 0. It counts by probing every existing token file outside the section, which
// the protocol permits, so it never waits for the section and works under any format. A
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
	locked, err := e.countLocked(resource)
	if err != nil {
		return 0, err
	}
	return max(0, size-locked), nil
}

// countLocked counts resource's locked tokens at every existing index by probing, outside the
// section. A missing res/<resource>/ has none; entries that are not canonical indices are not
// tokens and are skipped (PROTOCOL.md section 6).
func (e *Engine) countLocked(resource string) (int, error) {
	dir := filepath.Join(store.ResDir, resource)
	if err := store.CheckEntry(filepath.Join(e.st.Dir(), dir), true); err != nil {
		return 0, err
	}
	names, err := listNames(filepath.Join(e.st.Dir(), dir))
	if err != nil {
		return 0, err
	}
	locked := 0
	for _, name := range names {
		if !tokenIndex(name) {
			continue
		}
		dead, err := e.st.Probe(dir, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if !dead {
			locked++
		}
	}
	return locked, nil
}

// listNames lists a directory of the layout, reading at most store.MaxEntries+1 names; a
// missing directory is empty, and more than store.MaxEntries names is a state error.
func listNames(dir string) ([]string, error) {
	d, err := os.Open(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(store.MaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > store.MaxEntries {
		return nil, &store.StateError{Reason: fmt.Sprintf("%s holds more than %d entries", dir, store.MaxEntries)}
	}
	return names, nil
}

// tokenIndex reports whether name is a token index: decimal, no leading zeros, in [0, 4095].
func tokenIndex(name string) bool {
	if name == "" || len(name) > 4 || (len(name) > 1 && name[0] == '0') {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	n, _ := strconv.Atoi(name)
	return n <= wire.MaxTokenIndex
}

// Snapshot reads the state directory outside the section, so that it works while another
// client holds (or is stopped inside) the section, and writes nothing. Only a state error
// that makes the directory unreadable is returned; a malformed FORMAT, an invalid
// configuration and malformed entries are reported in the Report.
func (e *Engine) Snapshot() (Report, error) {
	r := Report{StateDir: e.st.Dir(), Resources: map[string]ResourceInfo{}, Ignored: map[string]int{}}

	r.Format, r.FormatUnderstood, r.FormatError = e.readFormat()
	g, err := e.st.ReadGen()
	if err != nil {
		return Report{}, err
	}
	r.Gen, r.ReconciledNS = g.N, g.ReconciledNS

	if sizes, err := e.sizes(); err != nil {
		r.ConfigError = err.Error()
	} else {
		for _, name := range slices.Sorted(maps.Keys(sizes)) {
			locked, err := e.countLocked(name)
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
		if errors.Is(err, fs.ErrNotExist) {
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
		if errors.Is(err, fs.ErrNotExist) {
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
		if errors.Is(err, fs.ErrNotExist) {
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
		if errors.Is(err, fs.ErrNotExist) {
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

// readFormat reads FORMAT outside the section with the grammar of PROTOCOL.md section 7.6:
// `coreslot-protocol <n>`, n of 1 to 9 ASCII digits, an optional final LF. An absent FORMAT
// is n = 0 and understood (the first entry writes format 1).
func (e *Engine) readFormat() (n int, understood bool, malformed string) {
	body, err := e.readBody("", store.FormatFile)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, true, ""
	}
	if err != nil {
		return 0, false, err.Error()
	}
	digits, ok := bytes.CutPrefix(bytes.TrimSuffix(body, []byte("\n")), []byte("coreslot-protocol "))
	if !ok || len(digits) == 0 || len(digits) > 9 || len(bytes.Trim(digits, "0123456789")) > 0 {
		return 0, false, fmt.Sprintf("malformed FORMAT %q", body)
	}
	n, _ = strconv.Atoi(string(digits))
	return n, slices.Contains(formats(), n), ""
}

// sectionInfo probes admit.lock (one single-attempt entry, left at once) and reads its
// informational body, tolerating an empty, stale or malformed one (PROTOCOL.md section 7.7).
func (e *Engine) sectionInfo() (SectionInfo, error) {
	var info SectionInfo
	sec, entered, err := e.st.TryEnter()
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
