//go:build darwin

package memsample

import (
	"os"

	"golang.org/x/sys/unix"
)

const (
	pidPathInfo      = 11   // PROC_PIDPATHINFO
	pidPathInfoSize  = 4096 // PROC_PIDPATHINFO_MAXSIZE: 4 * MAXPATHLEN
	pidCoalitionInfo = 20   // PROC_PIDCOALITIONINFO, absent from the public SDK headers
)

// lookupVM is DockerVM over this user's processes; a test replaces it.
var lookupVM = func() VM { return dockerVM(listVMProcs, footprintMiB) }

// footprintMiB is the footprint of pid.
func footprintMiB(pid int) (int64, bool) {
	ri, ok := rusage(pid, rusageV4)
	return int64(ri[footprintAt] >> 20), ok
}

// vmFootprint is the footprint of pid while it is the process started at startNS.
func vmFootprint(pid int, startNS int64) (int64, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid || kp.Proc.P_starttime.Nano() != startNS {
		return 0, false
	}
	return footprintMiB(pid)
}

// listVMProcs reads the pid, start time and executable path of every process of this user, and the
// resource coalition of each one that runs a program DockerVM looks for. A process that is gone or
// that cannot be read has no path and is skipped, unless its kernel name says it may be one of
// those: then it is Unread.
func listVMProcs() ([]vmProc, bool) {
	table, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, false
	}
	uid := uint32(os.Getuid())
	var out []vmProc
	path := make([]byte, pidPathInfoSize)
	co := make([]byte, coalitionInfoSize)
	for _, p := range table {
		if p.Eproc.Ucred.Uid != uid {
			continue
		}
		pid := int(p.Proc.P_pid)
		// The call returns 0 and leaves the NUL-terminated path in the buffer (libproc takes its length
		// with strlen); a process that cannot be read leaves it empty or fails.
		clear(path[:1])
		if _, err := info(callPidInfo, pid, pidPathInfo, 0, path); err != nil || path[0] == 0 {
			if mayBeOfInterest(unix.ByteSliceToString(p.Proc.P_comm[:])) {
				out = append(out, vmProc{PID: pid, Unread: true})
			}
			continue
		}
		vp := vmProc{PID: pid, StartNS: p.Proc.P_starttime.Nano(), Exec: unix.ByteSliceToString(path)}
		if exeIs(vp.Exec, dockerBackendExe) || isVMExe(vp.Exec) {
			cn, err := info(callPidInfo, pid, pidCoalitionInfo, 0, co)
			var ok bool
			if vp.Coalition, ok = parseCoalition(co, cn); err != nil || !ok {
				vp.Unread = true
			}
		}
		out = append(out, vp)
	}
	return out, true
}
