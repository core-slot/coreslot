//go:build unix && !linux

package main

import "golang.org/x/sys/unix"

// touch sets fd's times as the engine's refresh does outside Linux: futimes(fd, times).
func touch(fd int, ns int64) {
	tv := unix.NsecToTimeval(ns)
	_ = unix.Futimes(fd, []unix.Timeval{tv, tv})
}
