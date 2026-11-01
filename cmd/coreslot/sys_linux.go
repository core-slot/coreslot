//go:build linux

package main

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// fileCap is the most a soft RLIMIT_NOFILE can be set to below the hard limit; 0, none here.
func fileCap() uint64 { return 0 }

// fdDir is the directory that lists this process's open descriptors by number.
const fdDir = "/proc/self/fd"

// stopSelf stops the process with SIGSTOP sent to the calling thread, which the kernel delivers
// before the call returns: the process is stopped, and continued, before the caller goes on.
func stopSelf() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_ = unix.Tgkill(unix.Getpid(), unix.Gettid(), unix.SIGSTOP)
}

// maxRSSKiB converts a wait4 ru_maxrss to KiB: Linux reports it in kilobytes of 1024 bytes
// already (getrusage(2)).
func maxRSSKiB(maxrss int64) (kib int64, known bool) { return maxrss, true }
