//go:build darwin

package memsample

import (
	"os"

	"golang.org/x/sys/unix"
)

// The part of struct proc_bsdinfo (PROC_PIDTBSDINFO, sys/proc_info.h) that names a process, with
// MAXCOMLEN 16: after the eleven uint32 fields from pbi_flags to pbi_svgid and rfu_1, pbi_comm[16]
// is at offset 48 and pbi_name[2 * MAXCOMLEN] at 64; sizeof(struct proc_bsdinfo) is 136.
const (
	pidTBSDInfo = 3   // PROC_PIDTBSDINFO
	bsdInfoSize = 136 // PROC_PIDTBSDINFO_SIZE
	commAt      = 48  // pbi_comm
	commLen     = 16  // MAXCOMLEN
	nameAt      = 64  // pbi_name: empty if no name is registered
	nameLen     = 32  // 2 * MAXCOMLEN
)

// residentAt is ri_resident_size in a rusage_info_v4, in 64-bit words after the 16-byte ri_uuid.
const residentAt = 8

// procName is pid's pbi_name, or its pbi_comm when that is empty. PROC_PIDT_SHORTBSDINFO is not
// used: its name is 15 bytes, and no VM process name can match in that.
func procName(pid int) (string, bool) {
	var b [bsdInfoSize]byte
	if n, err := info(callPidInfo, pid, pidTBSDInfo, 0, b[:]); err != nil || n < bsdInfoSize {
		return "", false
	}
	if name := unix.ByteSliceToString(b[nameAt : nameAt+nameLen]); name != "" {
		return name, true
	}
	return unix.ByteSliceToString(b[commAt : commAt+commLen]), true
}

// Sweep reads the footprint, resident size, CPU time and name of every process of this user, and
// returns the n with the largest footprint and the Docker VM's process (the largest of the names
// vmNames matches). A process that is gone or that cannot be read is skipped; ok is false when the
// table cannot be listed or no process could be read.
func Sweep(n int) (top []ProcMem, vm *ProcMem, ok bool) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, nil, false
	}
	var pids []procRef
	uid := uint32(os.Getuid())
	for _, p := range procs {
		if p.Eproc.Ucred.Uid == uid {
			pids = append(pids, procRef{int(p.Proc.P_pid), p.Proc.P_starttime.Nano()})
		}
	}
	return sweepPIDs(pids, n)
}

// procRef is a process of the table: its pid and start time.
type procRef struct {
	pid     int
	startNS int64
}

// sweepPIDs is Sweep over the processes in refs.
func sweepPIDs(refs []procRef, n int) (top []ProcMem, vm *ProcMem, ok bool) {
	tb, err := timebase()
	if err != nil {
		return nil, nil, false
	}
	all := make([]ProcMem, 0, len(refs))
	for _, ref := range refs {
		ri, rok := rusage(ref.pid, rusageV4)
		name, nok := procName(ref.pid)
		if !rok || !nok {
			continue // gone, or another user's (EPERM)
		}
		cpu := (ri[userAt] + ri[sysAt]) * uint64(tb[0]) / uint64(tb[1])
		all = append(all, ProcMem{Name: name, FootprintMiB: int64(ri[footprintAt] >> 20), ResidentMiB: int64(ri[residentAt] >> 20),
			CPUS: float64(cpu) / 1e9, StartNS: ref.startNS})
	}
	if len(all) == 0 {
		return nil, nil, false
	}
	top, vm = rank(all, n)
	return top, vm, true
}
