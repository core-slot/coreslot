//go:build unix && !darwin && !linux

package main

import (
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

// load1 is the one-minute load average; this platform does not report it here.
func load1() float64 { return 0 }

// fileCap is the most a soft RLIMIT_NOFILE can be set to below the hard limit; 0, none here.
func fileCap() uint64 { return 0 }

// fdDir is the directory that lists this process's open descriptors by number; none is known
// here. A BSD's /dev/fd lists only 0 to 2 unless fdescfs is mounted, which would understate them.
const fdDir = ""

// stopSelf stops the process with SIGSTOP; a signal a process sends itself is delivered before
// kill returns.
func stopSelf() { _ = unix.Kill(unix.Getpid(), unix.SIGSTOP) }

// maxRSSKiB converts a wait4 ru_maxrss to KiB. FreeBSD, NetBSD, OpenBSD and DragonFly report it
// in kilobytes, as Linux does (their getrusage(2)). Elsewhere (Solaris, illumos, AIX) its unit is
// not established here, so the run log omits it rather than guess.
func maxRSSKiB(maxrss int64) (kib int64, known bool) {
	switch runtime.GOOS {
	case "freebsd", "netbsd", "openbsd", "dragonfly":
		return maxrss, true
	}
	return 0, false
}

// clampPriority leaves cmd as it is: on this platform, lowerPriority's nice demotes a preemptible command.
func clampPriority(*exec.Cmd) string { return "" }
