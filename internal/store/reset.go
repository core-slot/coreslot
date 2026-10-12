//go:build unix

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// LiveState is what a reset found alive and therefore refused to remove.
type LiveState struct {
	Holds   []string       // names of holds whose lock some description holds
	Tickets []string       // names of tickets whose lock some description holds
	Tokens  map[string]int // per resource, the locked tokens at every existing index
	// ForeignFormat is set when FORMAT is well-formed and names a format this client does not
	// implement: the entries of that format are not this client's to interpret or remove, and
	// its waiters are not visible here.
	ForeignFormat *int
}

// Empty reports that nothing is alive: the state may be reset.
func (l LiveState) Empty() bool {
	return len(l.Holds) == 0 && len(l.Tickets) == 0 && len(l.Tokens) == 0 && l.ForeignFormat == nil
}

// Reset empties the state directory of everything a reset removes (PROTOCOL.md section 6.2's
// `doctor --reset`), inside the section the caller entered: every entry of queue/, holds/ and
// cancel/ and of log/, then degraded, gen and FORMAT. It keeps admit.lock, res/ and config.toml
// (the format-stable files whose inodes live clients may hold open, and the configuration) and
// the four directories themselves.
//
// It first checks that nothing is alive: no hold or ticket whose lock is held and no locked
// token of any resource under res/, whether or not the configuration declares it. When anything
// is alive it removes nothing and returns it. An entry that is not a regular file of the user
// (a stray) is no client's and is removed with the rest; one that cannot be removed (a
// directory that is not empty) is an error, reported after the other removals have been tried.
// A FORMAT that is well-formed but not among implemented belongs to a newer client whose
// waiters this client cannot see: the reset refuses (ForeignFormat). A missing or malformed
// FORMAT does not stop it; a reset is what repairs a malformed one. It does not read the
// configuration.
//
// A reset that refuses, or that fails before it removes anything, writes nothing: it reads
// FORMAT, probes liveness and applies the foreign-format gate first, and only when every check
// has passed, immediately before the first removal, records itself in admit.lock's body, as
// every entry that understands the directory does, so that status names it while it works.
func (sec *Section) Reset(implemented []int) (live LiveState, removed int, err error) {
	if err := sec.live(); err != nil {
		return live, 0, err
	}
	s := sec.s
	if n, present, ferr := s.ReadFormat(); ferr == nil && present && !slices.Contains(implemented, n) {
		live.ForeignFormat = &n
		return live, 0, nil
	}
	resources, err := readNamesIfExist(s.path(ResDir))
	if err != nil {
		return live, 0, err
	}
	for _, name := range resources {
		if spec.ValidateName(name) != nil {
			continue
		}
		n, err := s.countLocked(name)
		if err != nil {
			return live, 0, err
		}
		if n > 0 {
			if live.Tokens == nil {
				live.Tokens = map[string]int{}
			}
			live.Tokens[name] = n
		}
	}
	listed := map[string][]string{}
	for _, dir := range []string{QueueDir, HoldsDir, CancelDir, LogDir} {
		if err := CheckEntry(s.path(dir), true); err != nil {
			return live, 0, err
		}
		names, err := readNamesIfExist(s.path(dir))
		if err != nil {
			return live, 0, err
		}
		listed[dir] = names
	}
	for _, dir := range []string{QueueDir, HoldsDir} {
		for _, name := range listed[dir] {
			if _, ok := wire.ParseName(name); !ok {
				continue
			}
			dead, err := s.Probe(dir, name)
			var se *StateError
			if errors.Is(err, fs.ErrNotExist) || errors.As(err, &se) {
				continue // gone, or a stray no client owns
			}
			if err != nil {
				return live, 0, err
			}
			if dead {
				continue
			}
			if dir == QueueDir {
				live.Tickets = append(live.Tickets, name)
			} else {
				live.Holds = append(live.Holds, name)
			}
		}
	}
	if !live.Empty() {
		return live, 0, nil
	}
	// Every check has passed and the next step removes: only now does the reset write, and its
	// first write is this mark. A reset that refuses, or fails before this point, has written
	// nothing at all, not even admit.lock's informational body.
	if err := sec.markHolder(); err != nil {
		return live, 0, err
	}
	var failed []error
	remove := func(path string) {
		switch err := os.Remove(path); {
		case err == nil:
			removed++
		case errors.Is(err, fs.ErrNotExist):
		default:
			failed = append(failed, err)
		}
	}
	for _, dir := range []string{QueueDir, HoldsDir, CancelDir, LogDir} {
		for _, name := range listed[dir] {
			remove(s.path(dir, name))
		}
	}
	for _, name := range []string{DegradedFile, GenFile, FormatFile} {
		remove(s.path(name))
	}
	if len(failed) > 0 {
		return live, removed, fmt.Errorf("store: reset left %d entries behind: %w", len(failed), errors.Join(failed...))
	}
	return live, removed, nil
}

// readNamesIfExist lists a directory like readNames; a missing one is empty.
func readNamesIfExist(dir string) ([]string, error) {
	names, err := readNames(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return names, err
}
