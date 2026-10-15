//go:build unix

package store

import "golang.org/x/sys/unix"

// CheckFilesystem refuses a state directory on a network, FUSE or VM-shared filesystem
// (PROTOCOL.md section 4.3), identified on Linux by statfs's f_type magic.
func CheckFilesystem(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return stateErrorf("statfs %s: %w", dir, err)
	}
	// f_type is int32, int64 or uint32 depending on the architecture; every magic is 32 bits.
	magic := int64(uint32(st.Type))
	if refusedMagic(magic) {
		return stateErrorf("%s is on a filesystem of type %#x; coreslot needs a host-local filesystem", dir, magic)
	}
	return nil
}
