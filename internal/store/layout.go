//go:build unix

// Package store is coreslot's filesystem layer: the state directory layout and its policy
// (PROTOCOL.md section 4), the admission section (8), atomic publication (11), gen and FORMAT
// (7.6, 7.8), reconciliation (10.4), lock probing (6), holds, the degraded record (7.9) and the
// run log (20). It decides nothing about admission: the engine combines it with
// the pure state machine.
//
// Release is always by closing a descriptor; nothing in this package unlocks with LOCK_UN.
package store

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Names of the layout (PROTOCOL.md section 4.4).
const (
	FormatFile   = "FORMAT"
	AdmitLock    = "admit.lock"
	GenFile      = "gen"
	QueueDir     = "queue"
	HoldsDir     = "holds"
	CancelDir    = "cancel"
	DegradedFile = "degraded"
	WatchFile    = "watch.json"
	LogDir       = "log"
	RunLog       = "runs.jsonl"
	RunLogOld    = "runs.1.jsonl"
	ConfigFile   = "config.toml"
)

// MaxEntries is the most entries one directory of the layout may hold (section 4.4).
const MaxEntries = 65536

// maxEntries is MaxEntries; tests lower it.
var maxEntries = MaxEntries

// Store is an opened state directory. It is safe for concurrent use; a Section is not.
type Store struct {
	dir string
}

// Dir is the state directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(elem ...string) string {
	return filepath.Join(append([]string{s.dir}, elem...)...)
}

// Open opens the state directory dir. It checks the filesystem of dir's nearest existing
// ancestor before creating anything, creates dir (mode 0700, with missing parents) and admit.lock
// (mode 0600) when absent, and checks dir, its filesystem and every existing entry of the layout,
// config.toml included. It creates nothing else: FORMAT has not been read yet, and a client must
// not write into a directory whose format it does not understand, so queue/, holds/, cancel/ and
// log/ are created by the operation that first needs them. admit.lock is fixed for every format.
// Every failure is a *StateError.
func Open(dir string) (*Store, error) { return openDir(dir, true) }

// OpenReadOnly opens the state directory for a reader that must leave the machine as it found
// it (status, doctor): it applies the same checks as Open to every entry that exists, and
// creates nothing, not even the state directory, which may be missing (a machine where nothing
// has run yet; Exists reports it). Operations that write still work on the Store it returns,
// and create what they need as they do after Open.
func OpenReadOnly(dir string) (*Store, error) { return openDir(dir, false) }

// Exists reports whether the state directory is there.
func (s *Store) Exists() bool {
	_, err := os.Lstat(s.dir)
	return err == nil
}

func openDir(dir string, create bool) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, stateErrorf("state directory %q is not an absolute path", dir)
	}
	dir = filepath.Clean(dir)
	if err := checkFilesystem(NearestExisting(dir)); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	if !create {
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			return s, nil
		}
	} else if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, stateErrorf("cannot create the state directory: %w", err)
	}
	if err := CheckEntry(dir, true); err != nil {
		return nil, err
	}
	if err := checkFilesystem(dir); err != nil {
		return nil, err
	}
	for _, d := range []string{QueueDir, HoldsDir, CancelDir, LogDir} {
		if err := CheckEntry(s.path(d), true); err != nil {
			return nil, err
		}
	}
	if create {
		f, err := OpenFile(s.path(AdmitLock), os.O_RDWR|os.O_CREATE)
		if err != nil {
			return nil, asStateError(err)
		}
		f.Close()
	}
	for _, name := range []string{AdmitLock, FormatFile, GenFile, DegradedFile, WatchFile, ConfigFile,
		filepath.Join(LogDir, RunLog), filepath.Join(LogDir, RunLogOld)} {
		if err := CheckEntry(s.path(name), false); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// checkFilesystem is CheckFilesystem; tests replace it to observe what Open checks and when.
var checkFilesystem = CheckFilesystem

// NearestExisting returns dir, or its nearest ancestor that exists: the directory whose
// filesystem a missing state directory would be created on.
func NearestExisting(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

func asStateError(err error) error {
	var se *StateError
	if errors.As(err, &se) {
		return err
	}
	return &StateError{Reason: err.Error(), Err: err}
}

// ensureDir creates a directory of the layout with mode 0700 when absent; an existing entry is
// checked as section 4.2 requires.
func ensureDir(path string) error {
	if err := os.Mkdir(path, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return CheckEntry(path, true)
}

// readNames lists a directory, reading at most MaxEntries+1 names; more than MaxEntries is a
// state error (section 4.4).
func readNames(dir string) ([]string, error) {
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	var names []string
	for {
		batch, err := d.Readdirnames(min(1024, maxEntries+1-len(names)))
		names = append(names, batch...)
		if len(names) > maxEntries {
			return nil, stateErrorf("%s holds more than %d entries", dir, maxEntries)
		}
		if errors.Is(err, io.EOF) || (err == nil && len(batch) == 0) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
