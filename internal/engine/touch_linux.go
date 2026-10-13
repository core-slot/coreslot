//go:build linux

package engine

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// touch sets f's access and modification times to ns through the descriptor: futimens(fd,
// times), which is utimensat(fd, NULL, times, 0) on Linux.
func touch(f *os.File, ns int64) error {
	ts := [2]unix.Timespec{unix.NsecToTimespec(ns), unix.NsecToTimespec(ns)}
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno unix.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = unix.Syscall6(unix.SYS_UTIMENSAT, fd, 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0)
	}); err != nil {
		return err
	}
	if errno != 0 {
		return &os.PathError{Op: "futimens", Path: f.Name(), Err: errno}
	}
	return nil
}
