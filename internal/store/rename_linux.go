//go:build unix

package store

import "golang.org/x/sys/unix"

// renameNoReplaceOS is renameat2(RENAME_NOREPLACE): it fails with EEXIST when to exists, and
// with EINVAL or ENOSYS where the kernel or filesystem does not support it.
func renameNoReplaceOS(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}
