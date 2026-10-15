//go:build coreslot_bench

package main

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// touch sets fd's times as the engine's refresh does on Linux: utimensat(fd, NULL, times, 0).
func touch(fd int, ns int64) {
	ts := [2]unix.Timespec{unix.NsecToTimespec(ns), unix.NsecToTimespec(ns)}
	_, _, _ = unix.Syscall6(unix.SYS_UTIMENSAT, uintptr(fd), 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0)
}
