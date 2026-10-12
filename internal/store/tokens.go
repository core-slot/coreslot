//go:build unix

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strconv"
	"syscall"

	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// TokenSet is a set of held tokens: each file is an open description holding flock(LOCK_EX) on
// $S/res/<name>/<i>. Files are in ascending resource name, then ascending index, the order the
// descriptors are passed to a child (PROTOCOL.md section 17); Indices lists the same tokens per
// resource.
type TokenSet struct {
	Files   []*os.File
	Indices map[string][]int
}

// Close closes this process's descriptors of the set. It never unlocks: a child that inherited
// copies keeps the tokens held until it exits. Repeated calls only close again.
func (ts *TokenSet) Close() {
	for _, f := range ts.Files {
		f.Close()
	}
}

// CheckIdentity checks I1 for every held token: the held file's inode equals stat of its path,
// and the file is still linked. A mismatch is a *StateError (degraded mode).
func (ts *TokenSet) CheckIdentity() error {
	for _, f := range ts.Files {
		held, err := f.Stat()
		if err != nil {
			return stateErrorf("token %s: %v", f.Name(), err)
		}
		hst, ok := held.Sys().(*syscall.Stat_t)
		if !ok || hst.Nlink == 0 {
			return stateErrorf("token %s: the held file is no longer linked", f.Name())
		}
		now, err := os.Lstat(f.Name())
		if err != nil {
			return stateErrorf("token %s: %v", f.Name(), err)
		}
		if !os.SameFile(held, now) {
			return stateErrorf("token %s: the path names another inode than the held file", f.Name())
		}
	}
	return nil
}

func (s *Store) resPath(resource string) (string, error) {
	if err := spec.ValidateName(resource); err != nil {
		return "", err
	}
	return s.path(ResDir, resource), nil
}

// CountLocked counts the locked tokens of resource at every existing index, including indices
// at or above the current size (PROTOCOL.md section 6, I3): one LOCK_NB probe per token file,
// closed again at once. Entries that are not token indices are ignored.
func (sec *Section) CountLocked(resource string) (locked int, err error) {
	if err := sec.live(); err != nil {
		return 0, err
	}
	return sec.s.countLocked(resource)
}

// CountLockedProbe counts like CountLocked, outside the section: probing is permitted inside and
// outside it (PROTOCOL.md section 6), so a reader such as Free or status never waits for the
// section. A probe's momentary lock can only make another client see a token as locked.
func (s *Store) CountLockedProbe(resource string) (locked int, err error) {
	return s.countLocked(resource)
}

func (s *Store) countLocked(resource string) (locked int, err error) {
	d, err := s.resPath(resource)
	if err != nil {
		return 0, err
	}
	if err := CheckEntry(d, true); err != nil {
		return 0, err
	}
	names, err := readNames(d)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, name := range names {
		if _, ok := parseIndex(name); !ok {
			continue
		}
		l, err := ProbeLocked(d + "/" + name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if l {
			locked++
		}
	}
	return locked, nil
}

// TakeTokens takes up to take[r] tokens of each resource r with LOCK_EX|LOCK_NB, at indices
// below sizes[r] only, in ascending resource name and then ascending index, creating missing
// token files and skipping tokens that turn out locked. It returns the set it holds and how
// many it got of each resource; deciding whether that is enough is the caller's. On an error it
// closes everything it took and returns none.
func (sec *Section) TakeTokens(take map[string]int, sizes map[string]int) (*TokenSet, map[string]int, error) {
	if err := sec.live(); err != nil {
		return nil, nil, err
	}
	total := 0
	for name, n := range take {
		if err := spec.ValidateName(name); err != nil {
			return nil, nil, err
		}
		if n < 0 || n > spec.MaxPerResource {
			return nil, nil, fmt.Errorf("store: take %d tokens of %s, at most %d", n, name, spec.MaxPerResource)
		}
		total += n
	}
	if total > spec.MaxPerGrant {
		return nil, nil, fmt.Errorf("store: take %d tokens, at most %d in one grant", total, spec.MaxPerGrant)
	}
	ts := &TokenSet{Indices: map[string][]int{}}
	got := make(map[string]int, len(take))
	fail := func(err error) (*TokenSet, map[string]int, error) {
		ts.Close()
		return nil, nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(take)) {
		got[name] = 0
		want := take[name]
		if want == 0 {
			continue
		}
		d, _ := sec.s.resPath(name)
		if err := ensureDir(d); err != nil {
			return fail(err)
		}
		size := min(sizes[name], spec.MaxSize)
		for i := 0; i < size && got[name] < want; i++ {
			f, err := OpenFile(d+"/"+strconv.Itoa(i), os.O_RDONLY|os.O_CREATE)
			if err != nil {
				return fail(err)
			}
			ok, err := flockNB(f)
			if err != nil {
				f.Close()
				return fail(err)
			}
			if !ok {
				f.Close() // held by someone else: skip it
				continue
			}
			ts.Files = append(ts.Files, f)
			ts.Indices[name] = append(ts.Indices[name], i)
			got[name]++
		}
	}
	return ts, got, nil
}
