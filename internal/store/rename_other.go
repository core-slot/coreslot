//go:build unix && !darwin && !linux

package store

import "golang.org/x/sys/unix"

// renameNoReplaceOS reports ENOSYS on systems without a no-replace rename, so publication uses
// the link(2) + unlink(2) fallback (PROTOCOL.md section 11).
func renameNoReplaceOS(from, to string) error {
	return unix.ENOSYS
}
