//go:build unix

package store

import (
	"encoding/json"
	"errors"
	"io/fs"
)

// maxWatchModeBytes bounds watch-mode: a larger file reads as no override.
const maxWatchModeBytes = 4 << 10

type watchModeFile struct {
	Mode    string `json:"mode"`
	SetNS   int64  `json:"set_ns"`
	UntilNS int64  `json:"until_ns"`
}

// ReadWatchMode reads this machine's override of the watchdog's mode (PROTOCOL.md section 7.15):
// its mode, "observe" or "enforce", and when it ends on the host's wall clock (0: until removed).
// It needs no section: the file is replaced whole, never rewritten. A missing file, one above
// 4 KiB, one that is not a JSON object, an unknown mode or a negative until_ns is no override (ok
// false); only a file that cannot be read, such as one that breaks the filesystem policy (a
// *StateError), is an error.
func (s *Store) ReadWatchMode() (mode string, untilNS int64, ok bool, err error) {
	body, err := s.ReadSmall(WatchModeFile, maxWatchModeBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", 0, false, nil
	case err != nil:
		return "", 0, false, err
	case len(body) > maxWatchModeBytes:
		return "", 0, false, nil
	}
	var f watchModeFile
	if json.Unmarshal(body, &f) != nil || (f.Mode != "observe" && f.Mode != "enforce") || f.UntilNS < 0 {
		return "", 0, false, nil
	}
	return f.Mode, f.UntilNS, true, nil
}

// SetWatchMode replaces this machine's override inside the section with mode ("observe" or
// "enforce") until untilNS on the host's wall clock, 0 for until removed.
func (sec *Section) SetWatchMode(mode string, untilNS int64) error {
	if mode != "observe" && mode != "enforce" {
		return errors.New("store: watch mode " + mode + ": want observe or enforce")
	}
	body, err := json.Marshal(watchModeFile{Mode: mode, SetNS: NowNS(), UntilNS: max(untilNS, 0)})
	if err != nil {
		return err
	}
	return sec.replaceFile(WatchModeFile, body)
}

// RemoveWatchMode removes this machine's override inside the section; a missing file is not an
// error.
func (sec *Section) RemoveWatchMode() error {
	_, err := sec.removeEntry("", WatchModeFile)
	return err
}
