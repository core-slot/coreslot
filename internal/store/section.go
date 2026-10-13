//go:build unix

package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// Section entry backoff (PROTOCOL.md section 8): LOCK_NB, retried from 10 ms up to 250 ms.
const (
	enterFirstBackoff = 10 * time.Millisecond
	enterMaxBackoff   = 250 * time.Millisecond
)

// errLeft is returned by every Section method called after Leave.
var errLeft = errors.New("store: the admission section was left")

// lockHooks is the pair SetLockHooks installed; nil until a program installs one.
var lockHooks atomic.Pointer[hookPair]

type hookPair struct{ enter, leave func() }

// SetLockHooks installs enter and leave, which bracket every moment at which this process may
// hold a lock it takes only momentarily: admit.lock, from each try to take it to the close that
// leaves the section (each try of Enter, the try of TryEnter, the probe of SectionHeld), and the
// probe lock of every token, ticket and hold (ProbeLocked, Probe, CountLocked, CountLockedProbe,
// and the probe-before-unlink of reconciliation and of RemoveOwnTicket and ProbeAndRemoveHold).
// enter runs just before each try, and leave runs exactly once for each enter, right after a
// try that did not take the lock, or right after the descriptor that took it is closed. So the
// time between them never includes Enter's sleeps between tries, only the tries themselves and
// the time the lock is held, and every moment such a lock is held lies between an enter and its
// leave.
//
// The locks a process owns for a lifetime are not bracketed: its ticket while it waits, and its
// hold and tokens while its grant lives. It takes them inside the section, so their tries are
// bracketed by the section's own pair, but they outlive it, and a process stopped while it owns
// them only keeps what it was granted, which is intended.
//
// It is how the CLI defers its job-control stops while it holds such a lock (PROTOCOL.md section
// 8) without deferring them while it only waits to enter the section. A process stopped inside
// the section blocks every admission; one stopped holding a probe lock makes every other client
// count that token as locked, or that ticket or hold as live, for as long as it stays stopped,
// so a head whose minimum needs the token is never admitted although nobody is stopped inside
// the section. The pairs nest and may overlap: a probe runs inside the section, and one goroutine
// may probe while another holds the section, so the hooks must count rather than toggle, and
// must be safe for concurrent use. They must be cheap, since a snapshot probes every token, and
// must not take a lock of the state directory themselves. A program installs them once, before
// it first takes such a lock; the library never does, and without them nothing runs.
func SetLockHooks(enter, leave func()) {
	lockHooks.Store(&hookPair{enter: enter, leave: leave})
}

func hookEnter() {
	if h := lockHooks.Load(); h != nil {
		h.enter()
	}
}

func hookLeave() {
	if h := lockHooks.Load(); h != nil {
		h.leave()
	}
}

// Section is the admission critical section: this process holds flock(LOCK_EX) on admit.lock
// through its own open file description until Leave closes it. A Section is not safe for
// concurrent use. Within one process a second entry through another description is refused like
// any other process's, so a holder must not try to enter again before it leaves.
type Section struct {
	s *Store
	f *os.File
}

// Enter enters the admission section: it tries LOCK_EX|LOCK_NB on admit.lock and retries with
// backoff from 10 ms up to 250 ms, checking ctx between tries; it never sleeps inside flock.
// When ctx ends first it returns ctx.Err() having changed nothing.
//
// Entering changes nothing either: Format, which every entry calls before any change, rewrites
// admit.lock's informational body once the format is known to be understood.
func (s *Store) Enter(ctx context.Context) (*Section, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := OpenFile(s.path(AdmitLock), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	backoff := enterFirstBackoff
	busy := false
	for {
		hookEnter()
		ok, err := flockNB(f)
		if err != nil {
			f.Close()
			hookLeave()
			return nil, err
		}
		if ok {
			point("section.entered")
			return &Section{s: s, f: f}, nil
		}
		hookLeave() // the sleep before the next try is waiting, not holding
		if !busy {
			// Once per entry, at its first busy try, so that a test knows the entry is waiting.
			busy = true
			point("section.busy")
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
		backoff = min(2*backoff, enterMaxBackoff)
	}
}

// TryEnter makes one attempt to enter the admission section (a library Poll's single try).
func (s *Store) TryEnter() (*Section, bool, error) {
	f, err := OpenFile(s.path(AdmitLock), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, false, err
	}
	hookEnter()
	ok, err := flockNB(f)
	if err != nil || !ok {
		f.Close()
		hookLeave()
		return nil, false, err
	}
	point("section.entered")
	return &Section{s: s, f: f}, true, nil
}

// SectionHeld probes admit.lock without entering and without creating it: one single-attempt
// flock, released at once by closing. A missing admit.lock means nothing has ever entered, so
// the section is free. It is for readers (status) that must not write into the state directory.
// A probe that finds the section free holds it for that moment, so the lock hooks bracket it as
// they bracket an entry.
func (s *Store) SectionHeld() (bool, error) {
	f, err := OpenFile(s.path(AdmitLock), os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	hookEnter()
	ok, err := flockNB(f)
	f.Close()
	hookLeave()
	return !ok, err
}

// Leave leaves the section by closing the admit.lock descriptor, then runs the lock hooks' leave.
// It is idempotent. Every Section must be left: until it is, the hooks count it as held.
func (sec *Section) Leave() {
	if sec.f != nil {
		sec.f.Close()
		sec.f = nil
		hookLeave()
	}
}

// sideSectionWait bounds how long a Store-level write that needs the section (clearing the
// degraded record, rotating the run log) waits to enter it: the same 2 s a run waits to remove its
// hold after its command exits (PROTOCOL.md section 12.3).
const sideSectionWait = 2 * time.Second

// NotEnteredError is a Store-level write that did not enter the section: another client held it
// for longer than 2 s, or admit.lock could not be opened or locked. Err is Enter's error; the
// write was not made.
type NotEnteredError struct {
	Err error
}

func (e *NotEnteredError) Error() string {
	return "store: cannot enter the admission section: " + e.Err.Error()
}

func (e *NotEnteredError) Unwrap() error { return e.Err }

// inSideSection makes change in a section it enters for that alone, after Prepare. A section it
// cannot enter is a *NotEnteredError. A FORMAT that is not understood is a *FormatError: one that
// could not be read, written when missing, or parsed carries that error, which its message
// repeats; a well-formed one this client does not implement is refused. change is not called then.
func (s *Store) inSideSection(change func(*Section) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), sideSectionWait)
	defer cancel()
	sec, err := s.Enter(ctx)
	if err != nil {
		return &NotEnteredError{Err: err}
	}
	defer sec.Leave()
	n, understood, err := sec.Prepare(NowNS())
	switch {
	case !understood && err != nil:
		return &FormatError{Err: err}
	case !understood:
		return fmt.Errorf("store: %w", &FormatError{N: n, Suffix: "; not writing"})
	case err != nil:
		return err
	}
	return change(sec)
}

func (sec *Section) live() error {
	if sec.f == nil {
		return errLeft
	}
	return nil
}

// markHolder rewrites admit.lock's informational body in place, through the locked descriptor,
// as wire.EncodeAdmitLock writes it (PROTOCOL.md section 7.7).
func (sec *Section) markHolder() error {
	body, err := wire.EncodeAdmitLock(wire.AdmitLock{PID: os.Getpid(), EnteredNS: max(NowNS(), 0)})
	if err != nil {
		return err
	}
	if err := sec.f.Truncate(0); err != nil {
		return err
	}
	_, err = sec.f.WriteAt(body, 0)
	return err
}

// flockNB is tryFlock; the tests wrap it to check that the lock hooks bracket every lock this
// package takes only momentarily.
var flockNB = tryFlock

// tryFlock tries flock(LOCK_EX|LOCK_NB) on f: true when this description now holds the lock,
// false when another description holds it.
func tryFlock(f *os.File) (bool, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return false, err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) {
		for {
			ferr = unix.Flock(int(fd), unix.LOCK_EX|unix.LOCK_NB)
			if !errors.Is(ferr, unix.EINTR) {
				return
			}
		}
	}); err != nil {
		return false, err
	}
	if errors.Is(ferr, unix.EWOULDBLOCK) {
		return false, nil
	}
	if ferr != nil {
		return false, &os.PathError{Op: "flock", Path: f.Name(), Err: ferr}
	}
	return true, nil
}
