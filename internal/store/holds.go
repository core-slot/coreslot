//go:build unix

package store

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"

	"github.com/core-slot/coreslot/internal/wire"
)

// maxNameAttempts bounds the seq retries of a publication whose final name exists.
const maxNameAttempts = 1000

// PublishHold publishes h as a lock-carrying hold named from its start time, pid, seq and
// label (PROTOCOL.md section 7.4). When that name exists it takes this process's next seq and
// retries. It returns the locked descriptor, which the holder keeps open and passes to its
// command as fd 3, and the name it published.
func (sec *Section) PublishHold(h wire.Hold, seq int) (*os.File, string, error) {
	body, err := wire.Encode(h)
	if err != nil {
		return nil, "", err
	}
	f, name, _, err := sec.PublishNamed(HoldsDir, body, seq, NextSeq, func(seq int) string {
		return wire.HoldName(h.StartNS, h.PID, seq, h.Label)
	})
	return f, name, err
}

// RewriteHold replaces the body of the hold that f holds at path with h, in place: the lock lives
// on the inode, so the file is never replaced. The body is written in one write at offset 0,
// padded with spaces to the length it replaces so that the file never shrinks, and POSIX makes a
// write and a read of a regular file atomic with respect to each other. A path that no longer
// names f's file is a *StateError (I1).
func RewriteHold(f *os.File, path string, h wire.Hold) error {
	body, err := wire.Encode(h)
	if err != nil {
		return err
	}
	w, err := OpenFile(path, os.O_WRONLY)
	if err != nil {
		return asStateError(err)
	}
	defer w.Close()
	if err := CheckHeld(f, path); err != nil {
		return err
	}
	fi, err := w.Stat()
	if err != nil {
		return err
	}
	if held, err := f.Stat(); err != nil || !os.SameFile(held, fi) {
		return stateErrorf("%s: the path names another inode than the held file", path)
	}
	if pad := int(fi.Size()) - len(body); pad > 0 {
		body = append(body, bytes.Repeat([]byte{' '}, pad)...)
	}
	_, err = w.WriteAt(body, 0)
	return err
}

// CheckHeld checks that the file f holds is still the one at path (I1, PROTOCOL.md section 3):
// still linked (st_nlink > 0), and path, which must exist, names the same (st_dev, st_ino). A
// mismatch, a missing path included, is a *StateError: the state directory, or the entry, was
// replaced under the holder.
func CheckHeld(f *os.File, path string) error {
	held, err := f.Stat()
	if err != nil {
		return stateErrorf("%s: %w", path, err)
	}
	hst, ok := held.Sys().(*syscall.Stat_t)
	if !ok || hst.Nlink == 0 {
		return stateErrorf("%s: the held file is no longer linked", path)
	}
	now, err := os.Lstat(path)
	if err != nil {
		return stateErrorf("%s: %w", path, err)
	}
	if !os.SameFile(held, now) {
		return stateErrorf("%s: the path names another inode than the held file", path)
	}
	return nil
}

// PublishNamed publishes body as a lock-carrying entry of dir (QueueDir or HoldsDir) named
// nameFor(seq), as Publish does. When that name exists it takes the seq next returns and retries,
// up to 1000 names. It returns the locked descriptor, the name it published and the seq that name
// carries.
func (sec *Section) PublishNamed(dir string, body []byte, seq int, next func() int, nameFor func(seq int) string) (*os.File, string, int, error) {
	for attempt := 0; attempt < maxNameAttempts; attempt++ {
		name := nameFor(seq)
		f, err := sec.Publish(dir, name, body, true)
		if errors.Is(err, fs.ErrExist) {
			seq = next()
			continue
		}
		if err != nil {
			return nil, "", 0, err
		}
		return f, name, seq, nil
	}
	return nil, "", 0, fmt.Errorf("store: no free name in %s after %d attempts", dir, maxNameAttempts)
}

// ProbeAndRemoveHold probes the hold name and unlinks it, while holding the probe lock, only
// when the probe succeeds (PROTOCOL.md section 12.3): a hold some descendant still holds is
// kept. It reports whether it removed the hold; a hold that no longer exists is not an error.
// Like RemoveOwnTicket, it outlasts another client's momentary probe (removeOwnEntry).
func (sec *Section) ProbeAndRemoveHold(name string) (bool, error) {
	if err := sec.live(); err != nil {
		return false, err
	}
	if _, ok := wire.ParseName(name); !ok {
		return false, fmt.Errorf("store: %q is not a hold name", name)
	}
	removed, _, err := removeOwnEntry(sec.s.path(HoldsDir, name))
	return removed, err
}

// RemoveOwnTicket removes this client's own ticket name inside the section after the client
// closed its descriptor (admission, cancellation, withdrawal): it probes the ticket and unlinks
// it while holding the probe lock, only when the probe succeeds, so a ticket some description
// still locks is never removed. A ticket that no longer exists counts as removed by someone else:
// removed is false and err nil (PROTOCOL.md section 10.3). A stray entry under that name (not a
// regular file of the user) is left alone: removed false, err nil (section 4.2). The caller
// bumps gen when removed is true (section 7.8).
func (sec *Section) RemoveOwnTicket(name string) (removed bool, err error) {
	if err := sec.live(); err != nil {
		return false, err
	}
	if _, ok := wire.ParseName(name); !ok {
		return false, fmt.Errorf("store: %q is not a ticket name", name)
	}
	removed, _, err = removeOwnEntry(sec.s.path(QueueDir, name))
	var se *StateError
	if errors.As(err, &se) {
		return false, nil
	}
	return removed, err
}

// removeOwnEntry is removeIfDead for an entry its owner has just closed: its own ticket
// (RemoveOwnTicket) or its own hold (ProbeAndRemoveHold). The probe finds the entry locked
// although the owner has closed its descriptor when another client's momentary probe (a status
// snapshot, an election) holds the lock at that instant; the entry would then stay dead until a
// reconciliation, though PROTOCOL.md sections 12.1 step 7 and 12.3 have its owner unlink it. Such
// a probe lets go within microseconds, so the owner tries again, at most ownRemovalTries times
// with ownRemovalPause between tries: about a millisecond in the section at worst, when a
// description that outlives the owner's (a descendant still running, or a child forked before it
// exec'd) really keeps the entry locked and it is left in place.
func removeOwnEntry(path string) (removed, gone bool, err error) {
	for try := 1; ; try++ {
		removed, gone, err = removeIfDead(path)
		if removed || gone || err != nil || try == ownRemovalTries {
			return removed, gone, err
		}
		time.Sleep(ownRemovalPause)
	}
}

const (
	ownRemovalTries = 8
	ownRemovalPause = 200 * time.Microsecond
)

// RemoveCancel removes the cancel request for the ticket id inside the section, once its owner
// found it (PROTOCOL.md section 13.1). A missing request is not an error, and a stray entry
// under that name (not a regular file of the user) is left alone (section 4.2).
func (sec *Section) RemoveCancel(id wire.ID) (removed bool, err error) {
	if err := sec.live(); err != nil {
		return false, err
	}
	p := sec.s.path(CancelDir, id.String())
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if checkInfo(p, fi, false) != nil {
		return false, nil
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
