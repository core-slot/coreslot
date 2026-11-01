//go:build linux

package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

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

// oomScore is what a run raises its oom_score_adj to before it starts its command, which inherits
// it: the kernel's or systemd-oomd's OOM kill then takes a restartable coreslot job first.
const oomScore = 500

// raiseOOMScore raises this process's oom_score_adj to oomScore, never lowering a higher one.
// A score it cannot write (EACCES) stays as it is.
func raiseOOMScore() {
	const path = "/proc/self/oom_score_adj"
	if data, err := os.ReadFile(path); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && n >= oomScore {
			return
		}
	}
	_ = os.WriteFile(path, []byte(strconv.Itoa(oomScore)), 0)
}
