//go:build unix

package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"

	"github.com/core-slot/coreslot/internal/wire"
)

// maxTrimBytes bounds trim.json: a file above it reads as no request, and a rewrite that would
// pass it is refused.
const maxTrimBytes = 64 << 10

// ErrTrimFull is a request to trim that would make trim.json larger than 64 KiB.
var ErrTrimFull = errors.New("store: trim.json would pass 64 KiB")

// TrimRequest is one entry of trim.json: a request that a live hold's run lower its reservation to
// what the run uses, and who asked when.
type TrimRequest struct {
	RequestedNS  int64
	RequesterPID int
}

type trimEntry struct {
	RequestedNS  int64 `json:"requested_ns"`
	RequesterPID int   `json:"requester_pid"`
}

type trimFile struct {
	Holds map[string]json.RawMessage `json:"holds"`
}

// ReadTrim reads trim.json's requests, keyed by hold id. It needs no section: the file is replaced
// whole, never rewritten. A missing file, one above 64 KiB or one that is not a JSON object with
// an object under "holds" is no request; an entry that is not an object of two integers at least
// 0 under a well-formed hold id is skipped alone, and unknown keys are ignored. Only a file that
// cannot be read, such as one that breaks the filesystem policy (a *StateError), is an error.
func (s *Store) ReadTrim() (map[string]TrimRequest, error) {
	out := map[string]TrimRequest{}
	body, err := s.ReadSmall(TrimFile, maxTrimBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return out, nil
	case err != nil:
		return nil, err
	case len(body) > maxTrimBytes:
		return out, nil
	}
	var f trimFile
	if json.Unmarshal(body, &f) != nil {
		return out, nil
	}
	for id, raw := range f.Holds {
		var e trimEntry
		if _, ok := wire.ParseID(id); !ok || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) ||
			json.Unmarshal(raw, &e) != nil || e.RequestedNS < 0 || e.RequesterPID < 0 {
			continue
		}
		out[id] = TrimRequest(e)
	}
	return out, nil
}

// RequestTrim rewrites trim.json inside the section with req for the hold id and the entries it
// holds for the ids in listed, which the caller read from holds/; the others' holds are gone. It
// bumps no gen. A file that would pass 64 KiB is not written and is ErrTrimFull.
func (sec *Section) RequestTrim(id wire.ID, req TrimRequest, listed map[string]bool) error {
	cur, err := sec.s.ReadTrim()
	if err != nil {
		return err
	}
	holds := map[string]trimEntry{}
	for k, r := range cur {
		if listed[k] {
			holds[k] = trimEntry(r)
		}
	}
	holds[id.String()] = trimEntry(req)
	body, err := json.Marshal(struct {
		Holds map[string]trimEntry `json:"holds"`
	}{holds})
	if err != nil {
		return err
	}
	if len(body) > maxTrimBytes {
		return ErrTrimFull
	}
	return sec.replaceFile(TrimFile, body)
}
