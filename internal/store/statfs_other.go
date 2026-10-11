//go:build unix && !darwin && !linux

package store

import "golang.org/x/sys/unix"

// CheckFilesystem on other Unix systems only checks that the directory can be examined:
// PROTOCOL.md section 4.3 lists refused filesystems for macOS and Linux only.
func CheckFilesystem(dir string) error {
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		return stateErrorf("stat %s: %v", dir, err)
	}
	return nil
}
