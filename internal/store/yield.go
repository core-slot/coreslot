//go:build unix

package store

import (
	"encoding/json"
	"errors"
	"io/fs"

	"github.com/core-slot/coreslot/internal/wire"
)

const maxYieldBytes = 4 << 10

// Yield is the head's yield record, $S/yield.json (PROTOCOL.md section 7.17): the head that lets
// short jobs go first, when it started, the deadline it published, and the last short pass. The
// times are the host's wall clock.
type Yield struct {
	Head       string `json:"head"` // the head's ticket id
	StartNS    int64  `json:"start_ns"`
	UntilNS    int64  `json:"until_ns"`
	LastPassNS int64  `json:"last_pass_ns"`
}

// ReadYield reads the yield record, with no section. A file that is not a record is none (ok
// false); only a file that cannot be read, such as one that breaks the filesystem policy (a
// *StateError), is an error.
func (s *Store) ReadYield() (y Yield, ok bool, err error) {
	body, err := s.ReadSmall(YieldFile, maxYieldBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Yield{}, false, nil
	case err != nil:
		return Yield{}, false, err
	case len(body) > maxYieldBytes || json.Unmarshal(body, &y) != nil:
		return Yield{}, false, nil
	}
	if _, valid := wire.ParseID(y.Head); !valid || min(y.StartNS, y.UntilNS, y.LastPassNS) < 0 {
		return Yield{}, false, nil
	}
	return y, true, nil
}

// SetYield replaces the yield record inside the section.
func (sec *Section) SetYield(y Yield) error {
	body, err := json.Marshal(y)
	if err != nil {
		return err
	}
	return sec.replaceFile(YieldFile, body)
}

// RemoveYield removes the yield record inside the section; one already gone is no error.
func (sec *Section) RemoveYield() error {
	_, err := sec.removeEntry("", YieldFile)
	return err
}
