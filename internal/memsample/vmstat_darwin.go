//go:build darwin

package memsample

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

// The Mach calls below are libSystem functions that x/sys/unix does not wrap. They are reached
// as x/sys/unix reaches libSystem: through the runtime's syscall6 and the trampolines of
// vmstat_darwin_*.s, so that no cgo is needed.

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

var (
	libc_mach_host_self_trampoline_addr     uintptr
	libc_host_statistics64_trampoline_addr  uintptr
	libc_mach_timebase_info_trampoline_addr uintptr
)

//go:cgo_import_dynamic libc_mach_host_self mach_host_self "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_host_statistics64 host_statistics64 "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_timebase_info mach_timebase_info "/usr/lib/libSystem.B.dylib"

// hostPort is mach_host_self(), taken once: each call adds a send right to the port.
var hostPort = sync.OnceValue(func() uintptr {
	port, _, _ := syscall_syscall6(libc_mach_host_self_trampoline_addr, 0, 0, 0, 0, 0, 0)
	return port
})

// vmStats reads host_statistics64's HOST_VM_INFO64 buffer: the first 38 32-bit words of struct
// vm_statistics64, which every macOS fills.
func vmStats() (v [rev1Count / 2]uint64, err error) {
	count := uint32(rev1Count)
	kr, _, _ := syscall_syscall6(libc_host_statistics64_trampoline_addr, hostPort(), hostVMInfo64,
		uintptr(unsafe.Pointer(&v[0])), uintptr(unsafe.Pointer(&count)), 0, 0)
	if int32(kr) != 0 {
		return v, fmt.Errorf("host_statistics64: kern_return %d", int32(kr))
	}
	return v, nil
}

const (
	hostVMInfo64 = 4  // HOST_VM_INFO64
	rev1Count    = 38 // HOST_VM_INFO64_REV1_COUNT, in 32-bit words
)

// vmCounters reads the pages decompressed and swapped in since boot from host_statistics64.
func vmCounters() (decomp, swapIns uint64, err error) {
	const (
		decompAt  = 12 // decompressions, in 64-bit words
		swapInsAt = 14 // swapins
	)
	v, err := vmStats()
	if err != nil {
		return 0, 0, err
	}
	return v[decompAt], v[swapInsAt], nil
}

// Parts reads the pages of the machine's memory in MiB, from the vm_statistics64 buffer of
// host_statistics64, and the kernel's memory pressure level (1 normal, 2 warning, 4 critical).
func Parts() (parts map[string]int64, pressure int, ok bool) {
	// Offsets, in 32-bit words, of the natural_t fields of struct vm_statistics64 in
	// mach/vm_statistics.h: free_count 0, wire_count 3, purgeable_count 22, speculative_count 23,
	// compressor_page_count 32, external_page_count 34, internal_page_count 35.
	const (
		freeAt, wireAt, purgeableAt, speculativeAt, compressorAt, externalAt, internalAt = 0, 3, 22, 23, 32, 34, 35
	)
	v, err := vmStats()
	size, sok := sysctlNum("vm.pagesize")
	level, lok := sysctlNum("kern.memorystatus_vm_pressure_level")
	if err != nil || !sok || !lok {
		return nil, 0, false
	}
	w := (*[rev1Count]uint32)(unsafe.Pointer(&v[0]))
	mib := func(at int) int64 { return int64(uint64(w[at]) * size >> 20) }
	return map[string]int64{"wired": mib(wireAt), "compressor": mib(compressorAt), "internal": mib(internalAt),
		"free": mib(freeAt), "speculative": mib(speculativeAt), "purgeable": mib(purgeableAt),
		"external": mib(externalAt)}, int(level), true
}

// timebase is mach_timebase_info: a mach tick is numer/denom ns.
var timebase = sync.OnceValues(func() ([2]uint32, error) {
	var tb [2]uint32
	kr, _, _ := syscall_syscall6(libc_mach_timebase_info_trampoline_addr, uintptr(unsafe.Pointer(&tb)), 0, 0, 0, 0, 0)
	if int32(kr) != 0 || tb[1] == 0 {
		return tb, fmt.Errorf("mach_timebase_info: kern_return %d", int32(kr))
	}
	return tb, nil
})
