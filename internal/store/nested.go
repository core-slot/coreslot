//go:build unix

package store

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// TokenFile is one token file of a state directory, with the identity a descriptor of it
// carries.
type TokenFile struct {
	Resource string
	Index    int
	Path     string
	Dev, Ino uint64
}

// TokenFiles lists every token file of the state directory dir (PROTOCOL.md section 6) without
// opening it as a Store, so it creates nothing: a pass-through validating its inherited grant
// writes nothing under the state directory (section 18). dir and res/ must exist; they, every
// res/<name>/ and every token file must pass the section 4.2 policy, otherwise the error is a
// *StateError (or the error of the missing path). Entries that are not resource names or token
// indices are skipped.
func TokenFiles(dir string) ([]TokenFile, error) {
	dir = filepath.Clean(dir)
	res := filepath.Join(dir, ResDir)
	for _, d := range []string{dir, res} {
		if _, err := os.Lstat(d); err != nil {
			return nil, err
		}
		if err := CheckEntry(d, true); err != nil {
			return nil, err
		}
	}
	names, err := readNames(res)
	if err != nil {
		return nil, err
	}
	var out []TokenFile
	for _, name := range names {
		if spec.ValidateName(name) != nil {
			continue
		}
		rd := filepath.Join(res, name)
		if err := CheckEntry(rd, true); err != nil {
			return nil, err
		}
		idx, err := readNames(rd)
		if err != nil {
			return nil, err
		}
		for _, i := range idx {
			n, ok := parseIndex(i)
			if !ok {
				continue
			}
			p := filepath.Join(rd, i)
			fi, err := os.Lstat(p)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, asStateError(err)
			}
			if err := checkInfo(p, fi, false); err != nil {
				return nil, err
			}
			st := fi.Sys().(*syscall.Stat_t)
			out = append(out, TokenFile{Resource: name, Index: n, Path: p, Dev: uint64(st.Dev), Ino: uint64(st.Ino)})
		}
	}
	return out, nil
}

// ReadFormatAt is Store.ReadFormat for a state directory that is not opened as a Store: a
// pass-through reads FORMAT outside the section and creates nothing (PROTOCOL.md section 20).
func ReadFormatAt(dir string) (n int, present bool, err error) {
	return (&Store{dir: filepath.Clean(dir)}).ReadFormat()
}

// AppendRunAt is Store.AppendRun for a state directory that is not opened as a Store, as a
// pass-through appends its line (PROTOCOL.md section 20): it applies Open's checks to $S (it
// must exist, pass the section 4.2 policy and sit on an accepted filesystem) and to log/, and
// creates nothing but log/ and its file; never admit.lock or res/.
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
