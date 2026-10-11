//go:build unix && !linux

package engine

import (
	"os"

	"golang.org/x/sys/unix"
)

// touch sets f's access and modification times to ns through the descriptor (futimes, whose
// resolution is a microsecond).
func touch(f *os.File, ns int64) error {
	tv := unix.NsecToTimeval(ns)
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) {
		ferr = unix.Futimes(int(fd), []unix.Timeval{tv, tv})
	}); err != nil {
		return err
	}
	if ferr != nil {
		return &os.PathError{Op: "futimes", Path: f.Name(), Err: ferr}
	}
	return nil
}
