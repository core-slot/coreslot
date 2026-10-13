//go:build darwin

package main

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// load1 is the one-minute load average, from the vm.loadavg sysctl (struct loadavg: three
// 32-bit fixed-point values, then the scale as a long); 0 when it cannot be read.
func load1() float64 {
	b, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(b) < 24 {
		return 0
	}
	ld := binary.NativeEndian.Uint32(b[0:4])
	scale := binary.NativeEndian.Uint64(b[16:24])
	if scale == 0 {
		return 0
	}
	return float64(ld) / float64(scale)
}

// fileCap is the most a soft RLIMIT_NOFILE can be set to below the hard limit: macOS refuses a
// soft limit above kern.maxfilesperproc. 0 means no cap is known.
func fileCap() uint64 {
	n, err := unix.SysctlUint32("kern.maxfilesperproc")
	if err != nil {
		return 0
	}
	return uint64(n)
}

// fdDir is the directory that lists this process's open descriptors by number.
const fdDir = "/dev/fd"

// stopSelf stops the process with SIGSTOP; a signal a process sends itself is delivered before
// kill returns.
func stopSelf() { _ = unix.Kill(unix.Getpid(), unix.SIGSTOP) }

// maxRSSKiB converts a wait4 ru_maxrss to KiB: macOS reports it in bytes (getrusage(2)), unlike
// Linux and the BSDs.
func maxRSSKiB(maxrss int64) (kib int64, known bool) { return maxrss / 1024, true }
