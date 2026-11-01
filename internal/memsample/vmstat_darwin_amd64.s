// Trampolines to the libSystem functions of vmstat_darwin.go, in the pattern of x/sys/unix.

#include "textflag.h"

TEXT libc_mach_host_self_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_host_self(SB)
GLOBL	·libc_mach_host_self_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_host_self_trampoline_addr(SB)/8, $libc_mach_host_self_trampoline<>(SB)

TEXT libc_host_statistics64_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_host_statistics64(SB)
GLOBL	·libc_host_statistics64_trampoline_addr(SB), RODATA, $8
DATA	·libc_host_statistics64_trampoline_addr(SB)/8, $libc_host_statistics64_trampoline<>(SB)

TEXT libc_mach_timebase_info_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_timebase_info(SB)
GLOBL	·libc_mach_timebase_info_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_timebase_info_trampoline_addr(SB)/8, $libc_mach_timebase_info_trampoline<>(SB)
