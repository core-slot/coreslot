//go:build unix

package store

import (
	"encoding/json"
	"errors"
	"io/fs"
)

const maxWatchModeBytes = 4 << 10

type watchModeFile struct {
	Mode    string `json:"mode"`
	SetNS   int64  `json:"set_ns"`
	UntilNS int64  `json:"until_ns"`
}

// ReadWatchMode reads this machine's override of the watchdog's mode (PROTOCOL.md section 7.15).
// A file that is not a valid override is none (ok false); only a file that cannot be read, such as
// one that breaks the filesystem policy (a *StateError), is an error.
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

// SetWatchMode replaces this machine's override inside the section, or removes it for mode "".
func (sec *Section) SetWatchMode(mode string, untilNS int64) error {
	switch mode {
	case "":
		_, err := sec.removeEntry("", WatchModeFile)
		return err
	case "observe", "enforce":
	default:
		return errors.New("store: watch mode " + mode + ": want observe or enforce")
	}
	body, err := json.Marshal(watchModeFile{Mode: mode, SetNS: NowNS(), UntilNS: max(untilNS, 0)})
	if err != nil {
		return err
	}
	return sec.replaceFile(WatchModeFile, body)
}
