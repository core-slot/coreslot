//go:build unix

package store

import "golang.org/x/sys/unix"

// renameNoReplaceOS is renamex_np(RENAME_EXCL): it fails with EEXIST when to exists.
func renameNoReplaceOS(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
