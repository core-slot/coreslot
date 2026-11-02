//go:build unix

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/core-slot/coreslot/internal/wire"
)

// LiveState is what a reset found alive and therefore refused to remove.
type LiveState struct {
	Holds   []string // names of holds whose lock some description holds
	Tickets []string // names of tickets whose lock some description holds
}

// Empty reports that nothing is alive: the state may be reset.
func (l LiveState) Empty() bool { return len(l.Holds) == 0 && len(l.Tickets) == 0 }

// otherRes is the res/ directory a directory of another format may hold; a reset removes it.
const otherRes = "res"

// ResetDone is what a reset that went ahead did with the entries of log/.
type ResetDone struct {
	Removed    int   // entries removed, those of log/ included
	LogRemoved int   // entries of log/ removed: FORMAT named another format or could not be read
	LogKept    int   // entries of log/ kept: a repair, under FORMAT 2 or none yet
	FormatErr  error // why FORMAT could not be read, when it could not
}

// Reset recreates the state directory (`coreslot doctor --reset`) inside the section the caller
// entered, whatever format its FORMAT names, malformed or absent included: it removes every entry
// of queue/, holds/ and cancel/, degraded, gen and another format's res/ with its files, then
// writes FORMAT of the format written. Under FORMAT 2, or none yet (which the first client writes
// as 2), it is a repair and keeps the entries of log/; otherwise it removes them too. It keeps
// admit.lock and config.toml (the format-stable file whose inode live clients may hold open, and
// the configuration) and the four directories themselves. It does not read the configuration.
//
// It first checks that nothing is alive: no hold or ticket whose lock is held, by names of either
// format. When anything is alive it removes nothing and returns it. An entry that is not a
// regular file of the user (a stray) is no client's and is removed with the rest; one that
// cannot be removed (a directory that is not empty) is an error, reported after the other
// removals have been tried.
//
// A reset that refuses, or that fails before it removes anything, writes nothing: it probes
// liveness first, and only when every check has passed, immediately before the first removal,
// records itself in admit.lock's body, as every entry that understands the directory does, so
// that status names it while it works.
func (sec *Section) Reset() (live LiveState, done ResetDone, err error) {
	if err := sec.live(); err != nil {
		return live, done, err
	}
	s := sec.s
	listed := map[string][]string{}
	for _, dir := range []string{QueueDir, HoldsDir, CancelDir, LogDir} {
		if err := CheckEntry(s.path(dir), true); err != nil {
			return live, done, err
		}
		names, err := readNamesIfExist(s.path(dir))
		if err != nil {
			return live, done, err
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
				return live, done, err
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
		return live, done, nil
	}
	// Every check has passed and the next step removes: only now does the reset write, and its
	// first write is this mark. A reset that refuses, or fails before this point, has written
	// nothing at all, not even admit.lock's informational body.
	if err := sec.markHolder(); err != nil {
		return live, done, err
	}
	n, present, ferr := s.ReadFormat()
	keepLog := ferr == nil && (!present || n == Format)
	done.FormatErr = ferr
	var failed []error
	remove := func(path string) bool {
		switch err := os.Remove(path); {
		case err == nil:
			done.Removed++
			return true
		case errors.Is(err, fs.ErrNotExist):
		default:
			failed = append(failed, err)
		}
		return false
	}
	for _, dir := range []string{QueueDir, HoldsDir, CancelDir, LogDir} {
		for _, name := range listed[dir] {
			switch {
			case dir != LogDir:
				remove(s.path(dir, name))
			case keepLog:
				done.LogKept++
			case remove(s.path(dir, name)):
				done.LogRemoved++
			}
		}
	}
	for _, name := range []string{DegradedFile, GenFile, WatchFile} {
		remove(s.path(name))
	}
	if _, err := os.Lstat(s.path(otherRes)); err == nil {
		if err := os.RemoveAll(s.path(otherRes)); err != nil {
			failed = append(failed, err)
		} else {
			done.Removed++
		}
	}
	if err := sec.writeFormat(); err != nil {
		failed = append(failed, err)
	}
	if len(failed) > 0 {
		return live, done, &ResetIncompleteError{Left: len(failed), Err: errors.Join(failed...)}
	}
	return live, done, nil
}

// ResetIncompleteError is a reset that removed what it could and left Left entries behind: the
// removal is best effort, so every other entry was still removed.
type ResetIncompleteError struct {
	Left int
	Err  error // the removal errors, joined
}

func (e *ResetIncompleteError) Error() string {
	return fmt.Sprintf("store: reset left %d entries behind: %v", e.Left, e.Err)
}

func (e *ResetIncompleteError) Unwrap() error { return e.Err }

// readNamesIfExist lists a directory like readNames; a missing one is empty.
func readNamesIfExist(dir string) ([]string, error) {
	names, err := readNames(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return names, err
}
