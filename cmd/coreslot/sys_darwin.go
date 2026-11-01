//go:build darwin

package main

import "golang.org/x/sys/unix"

// fdDir is the directory that lists this process's open descriptors by number.
const fdDir = "/dev/fd"

// stopSelf stops the process with SIGSTOP; a signal a process sends itself is delivered before
// kill returns.
func stopSelf() { _ = unix.Kill(unix.Getpid(), unix.SIGSTOP) }

// maxRSSKiB converts a wait4 ru_maxrss to KiB: macOS reports it in bytes (getrusage(2)), unlike
// Linux and the BSDs.
func maxRSSKiB(maxrss int64) (kib int64, known bool) { return maxrss / 1024, true }
