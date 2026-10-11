//go:build unix

package store

import (
	"bytes"

	"golang.org/x/sys/unix"
)

// CheckFilesystem refuses a state directory on a network, FUSE or VM-shared filesystem
// (PROTOCOL.md section 4.3), identified on macOS by statfs's f_fstypename.
func CheckFilesystem(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return stateErrorf("statfs %s: %v", dir, err)
	}
	name := string(bytes.TrimRight(st.Fstypename[:], "\x00"))
	if refusedType(name) {
		return stateErrorf("%s is on a %s filesystem; coreslot needs a host-local filesystem", dir, name)
	}
	return nil
}
