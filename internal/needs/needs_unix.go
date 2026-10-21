//go:build unix

package needs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/BurntSushi/toml"
	"golang.org/x/sys/unix"
)

var tmpSeq atomic.Uint64

// Update edits the needs file at path under an exclusive lock: it reads the file (a missing one
// is empty), hands edit its [needs] table, one map per label, and replaces the file with the
// result. A symbolic link is followed and the file it names is replaced, so the link stays a
// link; the lock is <real path>.lock, which every link to the file shares. The file keeps its
// mode, and is created 0600. A malformed file is an error and is left as it is, as it is when
// edit fails. Keys Update does not know are kept; comments are not.
func Update(path string, edit func(entries map[string]map[string]any) error) error {
	real := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		real = r
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	lock, err := os.OpenFile(real+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close() // releases the lock
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", lock.Name(), err)
	}

	perm := fs.FileMode(0o600)
	raw := map[string]any{}
	data, err := os.ReadFile(real)
	switch {
	case err == nil:
		fi, err := os.Stat(real)
		if err != nil {
			return err
		}
		perm = fi.Mode().Perm()
		if _, err := toml.Decode(string(data), &raw); err != nil {
			return fmt.Errorf("%s: %w", real, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	tbl, err := needsTable(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", real, err)
	}
	entries := make(map[string]map[string]any, len(tbl))
	for label, v := range tbl {
		e, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: [needs.%q] is not a table", real, label)
		}
		entries[label] = e
	}
	if err := edit(entries); err != nil {
		return err
	}
	raw["needs"] = entries

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(raw); err != nil {
		return err
	}
	return replace(real, buf.Bytes(), perm)
}

// replace writes data to a temporary file beside path, syncs it and renames it over path. The
// name, ".needs-<pid>-<n>.tmp", is outside the state directory's ".tmp-" grammar, whose files
// reconciliation removes.
func replace(path string, data []byte, perm fs.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".needs-%d-%d.tmp", os.Getpid(), tmpSeq.Add(1)))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	err = f.Chmod(perm) // the umask may have narrowed it
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
