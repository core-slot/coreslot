//go:build unix

package store

import (
	"context"
	"errors"
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

// sectionHooks is the pair SetSectionHooks installed; nil until a program installs one.
var sectionHooks atomic.Pointer[hookPair]

type hookPair struct{ enter, leave func() }

// SetSectionHooks installs enter and leave, which bracket every moment at which this process may
// hold flock on admit.lock: enter runs just before each try to take it (each try of Enter, the
// try of TryEnter, the probe of SectionHeld), and leave runs exactly once for each enter, right
// after a try that did not take the lock, or right after the descriptor that took it is closed
// (Leave, or the probe's close). So the time between them never includes Enter's sleeps between
// tries, only the tries themselves and the time spent inside the section, and every moment the
// lock is held, from the try that takes it on, lies between an enter and its leave.
//
// It is how the CLI defers its job-control stops while it holds the section (PROTOCOL.md section
// 8) without deferring them while it only waits to enter. The pairs nest and may overlap: one
// goroutine may probe while another holds the section, so the hooks must count rather than
// toggle, and must be safe for concurrent use. They must not enter the section themselves. A
// program installs them once, before it first enters; the library never does, and without them
// nothing runs.
func SetSectionHooks(enter, leave func()) {
	sectionHooks.Store(&hookPair{enter: enter, leave: leave})
}

func hookEnter() {
	if h := sectionHooks.Load(); h != nil {
		h.enter()
	}
}

func hookLeave() {
	if h := sectionHooks.Load(); h != nil {
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
// A probe that finds the section free holds it for that moment, so the section hooks bracket it
// as they bracket an entry.
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

// Leave leaves the section by closing the admit.lock descriptor, then runs the section hooks'
// leave. It is idempotent. Every Section must be left: until it is, the hooks count it as held.
func (sec *Section) Leave() {
	if sec.f != nil {
		sec.f.Close()
		sec.f = nil
		hookLeave()
	}
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

// flockNB is tryFlock; the tests wrap it to check that the section hooks bracket every time
// admit.lock is taken.
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
