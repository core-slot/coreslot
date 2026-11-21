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

type trimFile struct {
	Holds map[string]json.RawMessage `json:"holds"`
}

// ReadTrim reads the ids of the holds trim.json asks to trim. It needs no section: the file is
// replaced whole, never rewritten. A missing file, one above 64 KiB or one that is not a JSON
// object with an object under "holds" is no request; an entry under a key that is not a hold id,
// or whose value is not an object, is skipped alone, and the keys inside an entry are ignored. Only
// a file that cannot be read, such as one that breaks the filesystem policy (a *StateError), is an
// error.
func (s *Store) ReadTrim() (map[string]bool, error) {
	out := map[string]bool{}
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
	for id, raw := range f.Holds { // raw is valid JSON: an object starts with {
		if _, ok := wire.ParseID(id); ok && bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
			out[id] = true
		}
	}
	return out, nil
}

// RequestTrim rewrites trim.json inside the section with a request for the hold id and those it
// holds for the ids in listed, which the caller read from holds/; the others' holds are gone. It
// bumps no gen. A file that would pass 64 KiB is not written and is ErrTrimFull.
func (sec *Section) RequestTrim(id wire.ID, listed map[string]bool) error {
	cur, err := sec.s.ReadTrim()
	if err != nil {
		return err
	}
	holds := map[string]struct{}{id.String(): {}}
	for k := range cur {
		if listed[k] {
			holds[k] = struct{}{}
		}
	}
	body, err := json.Marshal(struct {
		Holds map[string]struct{} `json:"holds"`
	}{holds})
	if err != nil {
		return err
	}
	if len(body) > maxTrimBytes {
		return ErrTrimFull
	}
	return sec.replaceFile(TrimFile, body)
}
