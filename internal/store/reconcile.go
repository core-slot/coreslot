//go:build unix

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// Reconciliation period and temporary-file age (PROTOCOL.md section 10.4).
const (
	ReconcileEvery = 30 * time.Second
	TmpMaxAge      = 30 * time.Second
)

// Entry is a directory entry whose name matches its directory's grammar.
type Entry struct {
	Name    string
	ID      wire.ID
	MtimeNS int64 // the entry's mtime, as a value to compare with earlier sightings, never a clock
}

// List lists QueueDir, HoldsDir or CancelDir: the entries whose names match the directory's
// grammar, in name order (which is (order key or start, pid, seq) order), and the number of other
// entries, which are ignored. Temporary files are neither listed nor counted, unless one fails the
// section 4.2 checks. An entry whose name matches but whose type, owner or mode fails those checks
// (a symlink, a directory, another user's or a group- or world-writable file) is ignored and
// counted like a malformed body: never trusted and never removed (PROTOCOL.md section 4.2). The
// directory itself failing the checks, or more than MaxEntries entries, is a *StateError.
func (s *Store) List(dir string) ([]Entry, int, error) {
	d, names, parse, err := s.readDir("List", dir)
	if err != nil || names == nil {
		return nil, 0, err
	}
	var entries []Entry
	ignored := 0
	for _, name := range names {
		id, ok := parse(name)
		if !ok && !isTmpName(name) {
			ignored++
			continue
		}
		p := filepath.Join(d, name)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed since the listing
		}
		if err != nil {
			return nil, 0, err
		}
		if err := checkInfo(p, fi, false); err != nil {
			ignored++ // a stray entry, whatever its name matches
			continue
		}
		if !ok {
			continue // a sound temporary file: neither listed nor counted
		}
		entries = append(entries, Entry{Name: name, ID: id, MtimeNS: fi.ModTime().UnixNano()})
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	return entries, ignored, nil
}

// readDir is what List and ListNames share: the checked directory's path, its names (nil when
// it does not exist) and the name grammar of dir. op names the caller in errors.
func (s *Store) readDir(op, dir string) (path string, names []string, parse func(string) (wire.ID, bool), err error) {
	if !slices.Contains(noReplaceDirs, dir) {
		return "", nil, nil, fmt.Errorf("store: %s of %q: not a directory of tickets, holds or cancel requests", op, dir)
	}
	path = s.path(dir)
	if err := CheckEntry(path, true); err != nil {
		return "", nil, nil, err
	}
	names, err = readNames(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, nil, nil
	}
	if err != nil {
		return "", nil, nil, err
	}
	if names == nil {
		names = []string{}
	}
	parse = wire.ParseName
	if dir == CancelDir {
		parse = wire.ParseID
	}
	return path, names, parse, nil
}

// ListNames lists the names of dir (QueueDir, HoldsDir or CancelDir) that match its grammar, in
// name order, from the directory listing alone: unlike List it reads no entry's metadata, so an
// entry that fails the section 4.2 checks is listed too and is found out when it is opened or
// probed (an error for which a caller's stray check holds). MtimeNS is 0. The directory itself
// failing the checks, or more than MaxEntries entries, is a *StateError.
func (s *Store) ListNames(dir string) ([]Entry, error) {
	_, names, parse, err := s.readDir("ListNames", dir)
	if err != nil || names == nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(names))
	for _, name := range names {
		if id, ok := parse(name); ok {
			entries = append(entries, Entry{Name: name, ID: id})
		}
	}
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return entries, nil
}

// Probe probes dir/name: it opens the file, tries LOCK_EX|LOCK_NB and closes it again at once.
// dead is true when the lock could be taken, so no description holds it. A missing entry
// returns an error matching fs.ErrNotExist. Probing is permitted inside and outside the section.
func (s *Store) Probe(dir, name string) (dead bool, err error) {
	if name == "" || name != filepath.Base(name) {
		return false, fmt.Errorf("store: Probe of %q: not an entry name", name)
	}
	locked, err := ProbeLocked(s.path(dir, name))
	return !locked, err
}

// ProbeLocked opens path afresh, through the filesystem policy, and reports whether another open
// file description holds its lock; a lock the probe obtains is released at once by closing
// (PROTOCOL.md section 6). It creates nothing.
func ProbeLocked(path string) (locked bool, err error) {
	f, err := OpenFile(path, os.O_RDONLY)
	if err != nil {
		return false, err
	}
	defer f.Close()
	ok, err := flockNB(f)
	return !ok, err
}

// removeIfDead probes path and, when the probe takes the lock, unlinks the file while still
// holding the probe lock, then closes it (PROTOCOL.md section 10.4 step 1). gone reports a file
// that no longer exists.
func removeIfDead(path string) (removed, gone bool, err error) {
	f, err := OpenFile(path, os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	defer f.Close()
	ok, err := flockNB(f)
	if err != nil || !ok {
		return false, false, err
	}
	point("remove.before-unlink")
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, false, err
	}
	return true, false, nil
}

// ReconcileIfDue reconciles (PROTOCOL.md section 10.4) when reconciled_ns in gen is more than
// 30 s older than nowNS, or more than 30 s in its future; otherwise it does nothing. It probes
// every ticket and hold and unlinks the dead ones while holding the probe lock, removes cancel
// requests whose ticket is gone and temporary files older than 30 s, and writes gen with
// reconciled_ns = nowNS, increasing gen when it removed anything. Entries that do not match
// their grammar, and stray entries of queue/, holds/ and cancel/ that fail the section 4.2
// checks, are left alone; a stray temporary file in the state directory itself is a *StateError.
// The caller is inside the section and reconciles before its own
// change; admission and token-only admission never call it.
func (sec *Section) ReconcileIfDue(nowNS int64) (removed int, err error) {
	if err := sec.live(); err != nil {
		return 0, err
	}
	g, err := sec.s.ReadGen()
	if err != nil {
		return 0, err
	}
	if age := time.Duration(nowNS - g.ReconciledNS); age <= ReconcileEvery && age >= -ReconcileEvery {
		return 0, nil
	}

	// 1. Dead tickets and holds.
	tickets := map[string]bool{} // ids of the tickets still present
	for _, dir := range []string{QueueDir, HoldsDir} {
		entries, _, err := sec.s.List(dir)
		if err != nil {
			return removed, err
		}
		for _, e := range entries {
			r, gone, err := removeIfDead(sec.s.path(dir, e.Name))
			var se *StateError
			if errors.As(err, &se) {
				continue // replaced by a stray entry since the listing: skipped, never removed
			}
			if err != nil {
				return removed, err
			}
			if r {
				removed++
			} else if !gone && dir == QueueDir {
				tickets[e.ID.String()] = true
			}
		}
	}

	// 2. Cancel requests whose ticket is gone. tickets holds only the entries of step 1 that are
	// live and not stray: a stray never protects a cancel request (PROTOCOL.md section 10.4).
	cancels, _, err := sec.s.List(CancelDir)
	if err != nil {
		return removed, err
	}
	for _, e := range cancels {
		if tickets[e.Name] {
			continue
		}
		if err := os.Remove(sec.s.path(CancelDir, e.Name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		removed++
	}

	// 3. Temporary files older than 30 s.
	for _, dir := range []string{"", QueueDir, HoldsDir, CancelDir} {
		d := sec.s.path(dir)
		names, err := readNames(d)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		for _, name := range names {
			if !isTmpName(name) {
				continue
			}
			p := filepath.Join(d, name)
			fi, err := os.Lstat(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return removed, err
			}
			if err := checkInfo(p, fi, false); err != nil {
				if dir == "" {
					return removed, err
				}
				continue // a stray entry of queue/, holds/ or cancel/: skipped, never removed
			}
			if time.Duration(nowNS-fi.ModTime().UnixNano()) <= TmpMaxAge {
				continue
			}
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return removed, err
			}
			removed++
		}
	}

	// 4. gen.
	g.ReconciledNS = nowNS
	if removed > 0 {
		g.N = nextGen(g.N)
	}
	return removed, sec.writeGen(g)
}
