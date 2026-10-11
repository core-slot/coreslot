//go:build unix

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Section entry backoff (PROTOCOL.md section 8): LOCK_NB, retried from 10 ms up to 250 ms.
const (
	enterFirstBackoff = 10 * time.Millisecond
	enterMaxBackoff   = 250 * time.Millisecond
)

// errLeft is returned by every Section method called after Leave.
var errLeft = errors.New("store: the admission section was left")

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
	for {
		ok, err := flockNB(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			point("section.entered")
			return &Section{s: s, f: f}, nil
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
	ok, err := flockNB(f)
	if err != nil || !ok {
		f.Close()
		return nil, false, err
	}
	point("section.entered")
	return &Section{s: s, f: f}, true, nil
}

// Leave leaves the section by closing the admit.lock descriptor. It is idempotent.
func (sec *Section) Leave() {
	if sec.f != nil {
		sec.f.Close()
		sec.f = nil
	}
}

func (sec *Section) live() error {
	if sec.f == nil {
		return errLeft
	}
	return nil
}

// markHolder rewrites admit.lock's informational body in place, through the locked descriptor,
// as {"pid": <pid>, "entered_ns": <wall clock>} (PROTOCOL.md section 7.7).
func (sec *Section) markHolder() error {
	body := fmt.Appendf(nil, `{"pid":%d,"entered_ns":%d}`, os.Getpid(), NowNS())
	if err := sec.f.Truncate(0); err != nil {
		return err
	}
	_, err := sec.f.WriteAt(body, 0)
	return err
}

// flockNB tries flock(LOCK_EX|LOCK_NB) on f: true when this description now holds the lock,
// false when another description holds it.
func flockNB(f *os.File) (bool, error) {
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
