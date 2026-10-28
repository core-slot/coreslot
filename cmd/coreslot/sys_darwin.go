//go:build darwin

package main

import (
	"encoding/binary"
	"os"
	"os/exec"

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

// taskpolicyPath is taskpolicy(8), which runs a command under a QoS clamp.
const taskpolicyPath = "/usr/sbin/taskpolicy"

// clampPriority makes cmd start under a utility QoS clamp, as `taskpolicy -c utility -- cmd`.
// macOS ignores nice for every thread with a QoS class, and libpthread gives every ordinary
// thread one, so lowerPriority alone leaves a preemptible command at its full priority. The clamp
// caps the priority of every thread of the command and of everything it starts, from its first
// instruction, terminal mode and --same-group included. taskpolicy sets the clamp and replaces
// itself with the command (posix_spawn with POSIX_SPAWN_SETEXEC), so the command keeps the pid,
// the process group, the environment and the inherited descriptors. The command sees its
// resolved path as argv[0]. A command that could not be resolved is left as it is, so its error
// stays coreslot's, and so is every command where taskpolicy is missing.
func clampPriority(cmd *exec.Cmd) {
	if cmd.Err != nil {
		return
	}
	if _, err := os.Stat(taskpolicyPath); err != nil {
		return
	}
	cmd.Args = append([]string{taskpolicyPath, "-c", "utility", "--", cmd.Path}, cmd.Args[1:]...)
	cmd.Path = taskpolicyPath
}
