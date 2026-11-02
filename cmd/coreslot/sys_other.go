//go:build unix && !darwin && !linux

package main

import "golang.org/x/sys/unix"

// fdDir is the directory that lists this process's open descriptors by number; none is known here.
const fdDir = ""

// stopSelf stops the process with SIGSTOP; a signal a process sends itself is delivered before
// kill returns.
func stopSelf() { _ = unix.Kill(unix.Getpid(), unix.SIGSTOP) }

// maxRSSKiB says that a wait4 ru_maxrss has no known unit here, so the run log omits it.
func maxRSSKiB(int64) (kib int64, known bool) { return 0, false }

// raiseOOMScore does nothing: no OOM score is known here.
func raiseOOMScore() {}
