//go:build unix

package store

import (
	"os"
	"path/filepath"
)

// ReadFormatAt is Store.ReadFormat for a state directory that is not opened as a Store: a
// pass-through reads FORMAT outside the section and creates nothing (PROTOCOL.md section 20).
func ReadFormatAt(dir string) (n int, present bool, err error) {
	return (&Store{dir: filepath.Clean(dir)}).ReadFormat()
}

// AppendRunAt is Store.AppendRun for a state directory that is not opened as a Store, as a
// pass-through appends its line (PROTOCOL.md section 20): it applies Open's checks to $S (it
// must exist, pass the section 4.2 policy and sit on an accepted filesystem) and to log/, and
// creates nothing but log/ and its file; never admit.lock.
func AppendRunAt(dir string, r RunRecord) error {
	dir = filepath.Clean(dir)
	if _, err := os.Lstat(dir); err != nil {
		return err
	}
	if err := CheckEntry(dir, true); err != nil {
		return err
	}
	if err := checkFilesystem(dir); err != nil {
		return err
	}
	s := &Store{dir: dir}
	if err := CheckEntry(s.path(LogDir), true); err != nil {
		return err
	}
	return s.AppendRun(r)
}
