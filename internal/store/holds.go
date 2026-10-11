//go:build unix

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// maxNameAttempts bounds the seq retries of a publication whose final name exists.
const maxNameAttempts = 1000

// PublishHold publishes h as a lock-carrying hold named from its start time, pid, seq and
// label (PROTOCOL.md section 7.4). When that name exists it takes this process's next seq and
// retries. It returns the locked descriptor, which the holder keeps open and passes to its
// command as fd 3, and the name it published.
func (sec *Section) PublishHold(h wire.Hold, pid, seq int) (*os.File, string, error) {
	body, err := wire.Encode(h)
	if err != nil {
		return nil, "", err
	}
	for attempt := 0; attempt < maxNameAttempts; attempt++ {
		name := wire.HoldName(h.StartNS, pid, seq, h.Label)
		f, err := sec.Publish(HoldsDir, name, body, true)
		if errors.Is(err, fs.ErrExist) {
			seq = NextSeq()
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return f, name, nil
	}
	return nil, "", fmt.Errorf("store: no free hold name after %d attempts", maxNameAttempts)
}

// ProbeAndRemoveHold probes the hold name and unlinks it, while holding the probe lock, only
// when the probe succeeds (PROTOCOL.md section 12.3): a hold some descendant still holds is
// kept. It reports whether it removed the hold; a hold that no longer exists is not an error.
func (sec *Section) ProbeAndRemoveHold(name string) (bool, error) {
	if err := sec.live(); err != nil {
		return false, err
	}
	if _, ok := wire.ParseName(name); !ok {
		return false, fmt.Errorf("store: %q is not a hold name", name)
	}
	removed, _, err := removeIfDead(sec.s.path(HoldsDir, name))
	return removed, err
}
