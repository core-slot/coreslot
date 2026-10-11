//go:build unix && !darwin && !linux

package store

// CheckFilesystem cannot tell a host-local filesystem from a network, FUSE or VM-shared one on
// this system (PROTOCOL.md section 4.3 lists the refused filesystems for macOS and Linux only),
// so it is always a state error and the caller fails open.
func CheckFilesystem(dir string) error {
	return stateErrorf("%s: cannot verify a host-local filesystem on this system", dir)
}
