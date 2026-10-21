//go:build darwin

package memsample

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Source names Tree's measure: the physical footprint, as footprint(1) reports it.
const Source = "footprint"

// Tree returns the summed physical footprint of root and its descendants, in KiB.
func Tree(root int) (kib int64, ok bool) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return 0, false
	}
	parents := make(map[int]int, len(procs))
	for _, p := range procs {
		parents[int(p.Proc.P_pid)] = int(p.Eproc.Ppid)
	}
	return treeSum(root, parents, footprint)
}

// footprint reads ri_phys_footprint of pid's rusage_info_v4 (proc_pid_rusage), in KiB.
func footprint(pid int) (int64, bool) {
	const (
		pidRusage   = 9  // PROC_INFO_CALL_PIDRUSAGE
		rusageV4    = 4  // RUSAGE_INFO_V4
		footprintAt = 72 // offset of ri_phys_footprint in rusage_info_v4
	)
	var buf [512]byte
	_, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, pidRusage, uintptr(pid), rusageV4, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return 0, false
	}
	return int64(binary.LittleEndian.Uint64(buf[footprintAt:]) >> 10), true
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
