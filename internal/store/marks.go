//go:build unix

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/core-slot/coreslot/internal/wire"
)

// Mark publishes the empty mark dir/<id> inside the section, without replacement (PROTOCOL.md
// section 7.18), where dir is QuietDir or NoStopDir and id names a ticket or a hold. already is true
// when the mark was there. Publishing a mark does not bump gen.
func (sec *Section) Mark(dir string, id wire.ID) (already bool, err error) {
	if !isMarkDir(dir) {
		return false, fmt.Errorf("store: Mark in %q: not a directory of marks", dir)
	}
	_, err = sec.Publish(dir, id.String(), nil, false)
	if errors.Is(err, fs.ErrExist) {
		return true, nil
	}
	return false, err
}

// Unmark removes the mark dir/<id> inside the section. A missing mark is not an error, and a stray
// entry under that name is left alone (section 4.2).
func (sec *Section) Unmark(dir string, id wire.ID) (removed bool, err error) {
	if !isMarkDir(dir) {
		return false, fmt.Errorf("store: Unmark in %q: not a directory of marks", dir)
	}
	return sec.removeByID(dir, id)
}

// Marks lists the ids dir marks, without the section; stray entries are ignored, never trusted.
func (s *Store) Marks(dir string) (map[string]bool, error) {
	if !isMarkDir(dir) {
		return nil, fmt.Errorf("store: Marks of %q: not a directory of marks", dir)
	}
	entries, _, err := s.List(dir)
	if err != nil {
		return nil, err
	}
	marks := make(map[string]bool, len(entries))
	for _, e := range entries {
		marks[e.Name] = true
	}
	return marks, nil
}

func isMarkDir(dir string) bool { return slices.Contains([]string{QuietDir, NoStopDir}, dir) }
