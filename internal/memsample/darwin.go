//go:build darwin

package memsample

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Tree returns the summed physical footprint of root and its descendants, in KiB, and the user,
// system and page-wait time of each, from one proc_pid_rusage per process: V6 where it carries
// page wait, V4 elsewhere.
func Tree(root int) (kib int64, times map[int]TreeTimes, ok bool) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	tb, terr := timebase()
	if err != nil || terr != nil {
		return 0, nil, false
	}
	parents := make(map[int]int, len(procs))
	for _, p := range procs {
		parents[int(p.Proc.P_pid)] = int(p.Eproc.Ppid)
	}
	ns := func(ticks uint64) int64 { return int64(ticks * uint64(tb[0]) / uint64(tb[1])) }
	has, flavor := pageWait(), rusageV4
	if has {
		flavor = rusageV6
	}
	return treeSum(root, parents, func(pid int) (int64, TreeTimes, bool) {
		ri, ok := rusage(pid, flavor)
		t := TreeTimes{UserNS: ns(ri[userAt]), SysNS: ns(ri[sysAt]), HasPageWait: has}
		if has {
			t.PageWaitNS = ns(ri[pageWaitAt])
		}
		return int64(ri[footprintAt] >> 10), t, ok
	})
}

// Listing reads the process table once: kern.proc.all's parent and start time of every process.
func Listing() (map[int]Proc, bool) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, false
	}
	out := make(map[int]Proc, len(procs))
	for _, p := range procs {
		out[int(p.Proc.P_pid)] = Proc{PPID: int(p.Eproc.Ppid), StartNS: p.Proc.P_starttime.Nano()}
	}
	return out, true
}

// The rusage_info flavors read, and the places of their fields in 64-bit words, after the 16-byte
// ri_uuid; V4's fields are V6's first ones.
const (
	rusageV4    = 4  // RUSAGE_INFO_V4
	rusageV6    = 6  // RUSAGE_INFO_V6
	userAt      = 2  // ri_user_time, in mach ticks
	sysAt       = 3  // ri_system_time, in mach ticks
	footprintAt = 9  // ri_phys_footprint, in bytes
	pageWaitAt  = 50 // ri_page_wait_time_mach, V6 only
)

// rusage reads pid's rusage_info of flavor (proc_pid_rusage); ok is false when pid is gone.
func rusage(pid, flavor int) (ri [64]uint64, ok bool) {
	const pidRusage = 9 // PROC_INFO_CALL_PIDRUSAGE
	//lint:ignore SA1019 x/sys/unix wraps no proc_pid_rusage; a libSystem call needs cgo or an assembly trampoline
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, pidRusage, uintptr(pid), uintptr(flavor), 0,
		uintptr(unsafe.Pointer(&ri[0])), unsafe.Sizeof(ri))
	return ri, errno == 0
}

// Headroom returns the memory the kernel can hand out without paging anything anonymous out, in
// MiB: free, speculative, purgeable and file-backed pageable pages, times the page size.
func Headroom() (mib int64, ok bool) {
	size, ok := sysctlNum("vm.pagesize")
	if !ok {
		return 0, false
	}
	var pages uint64
	for _, name := range []string{"vm.page_free_count", "vm.page_speculative_count",
		"vm.page_purgeable_count", "vm.page_pageable_external_count"} {
		n, ok := sysctlNum(name)
		if !ok {
			return 0, false
		}
		pages += n
	}
	return int64(pages * size >> 20), true
}

// sysctlNum reads a numeric sysctl of 4 or 8 bytes.
func sysctlNum(name string) (uint64, bool) {
	b, err := unix.SysctlRaw(name)
	switch {
	case err != nil:
		return 0, false
	case len(b) == 4:
		return uint64(binary.LittleEndian.Uint32(b)), true
	case len(b) == 8:
		return binary.LittleEndian.Uint64(b), true
	}
	return 0, false
}
