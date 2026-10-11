//go:build unix

// Package store is coreslot's filesystem layer: the state directory layout and its policy
// (PROTOCOL.md section 4), the admission section (8), atomic publication (11), gen and FORMAT
// (7.6, 7.8), reconciliation (10.4), tokens and lock counting (6), holds, the degraded record
// (7.9) and the run log (20). It decides nothing about admission: the engine combines it with
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
	"strconv"

	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// Names of the layout (PROTOCOL.md section 4.4).
const (
	FormatFile   = "FORMAT"
	AdmitLock    = "admit.lock"
	GenFile      = "gen"
	ResDir       = "res"
	QueueDir     = "queue"
	HoldsDir     = "holds"
	CancelDir    = "cancel"
	DegradedFile = "degraded"
	LogDir       = "log"
	RunLog       = "runs.jsonl"
	RunLogOld    = "runs.1.jsonl"
	ConfigFile   = "config.toml"
)

// MaxEntries is the most entries one directory of the layout may hold (section 4.4).
const MaxEntries = 65536

// Store is an opened state directory. It is safe for concurrent use; a Section is not.
type Store struct {
	dir string
}

// Dir is the state directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(elem ...string) string {
	return filepath.Join(append([]string{s.dir}, elem...)...)
}

// Open opens the state directory dir, creating it and the directories of the layout (mode
// 0700) and admit.lock (mode 0600) when absent, and checks dir, its filesystem and every
// existing entry of the layout, including each res/<name>/ and its token files, and
// config.toml. Every failure is a *StateError.
func Open(dir string) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, stateErrorf("state directory %q is not an absolute path", dir)
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, stateErrorf("cannot create the state directory: %v", err)
	}
	if err := CheckEntry(dir, true); err != nil {
		return nil, err
	}
	if err := CheckFilesystem(dir); err != nil {
		return nil, err
	}
	s := &Store{dir: dir}
	for _, d := range []string{ResDir, QueueDir, HoldsDir, CancelDir, LogDir} {
		if err := ensureDir(s.path(d)); err != nil {
			return nil, asStateError(err)
		}
	}
	f, err := OpenFile(s.path(AdmitLock), os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, asStateError(err)
	}
	f.Close()
	for _, name := range []string{FormatFile, GenFile, DegradedFile, ConfigFile,
		filepath.Join(LogDir, RunLog), filepath.Join(LogDir, RunLogOld)} {
		if err := CheckEntry(s.path(name), false); err != nil {
			return nil, err
		}
	}
	if err := s.checkTokens(); err != nil {
		return nil, asStateError(err)
	}
	return s, nil
}

// checkTokens checks every res/<name>/ directory and every token file in it. Entries whose
// names are not resource names or token indices are not part of the layout and are left alone.
func (s *Store) checkTokens() error {
	names, err := readNames(s.path(ResDir))
	if err != nil {
		return err
	}
	for _, name := range names {
		if spec.ValidateName(name) != nil {
			continue
		}
		rd := s.path(ResDir, name)
		if err := CheckEntry(rd, true); err != nil {
			return err
		}
		idx, err := readNames(rd)
		if err != nil {
			return err
		}
		for _, i := range idx {
			if _, ok := parseIndex(i); !ok {
				continue
			}
			if err := CheckEntry(filepath.Join(rd, i), false); err != nil {
				return err
			}
		}
	}
	return nil
}

func asStateError(err error) error {
	var se *StateError
	if errors.As(err, &se) {
		return err
	}
	return &StateError{Reason: err.Error()}
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
		batch, err := d.Readdirnames(1024)
		names = append(names, batch...)
		if len(names) > MaxEntries {
			return nil, stateErrorf("%s holds more than %d entries", dir, MaxEntries)
		}
		if errors.Is(err, io.EOF) || (err == nil && len(batch) == 0) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// parseIndex parses a token file name: a decimal index in [0, 4095] without leading zeros.
func parseIndex(name string) (int, bool) {
	if name == "" || len(name) > 4 || (len(name) > 1 && name[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(name)
	if err != nil || n > wire.MaxTokenIndex {
		return 0, false
	}
	return n, true
}
