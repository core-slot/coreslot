//go:build coreslot_bench

package main

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

func host() hostInfo {
	model, _ := unix.Sysctl("hw.model")
	cpus, _ := unix.SysctlUint32("hw.logicalcpu")
	ver, _ := unix.Sysctl("kern.osproductversion")
	build, _ := unix.Sysctl("kern.osversion")
	var u unix.Utsname
	_ = unix.Uname(&u)
	return hostInfo{Model: model, CPUs: int(cpus),
		OS: fmt.Sprintf("macOS %s (%s), Darwin %s", ver, build, unix.ByteSliceToString(u.Release[:]))}
}

// loadAvg reads vm.loadavg: struct loadavg { fixpt_t ldavg[3]; long fscale; }.
func loadAvg() string {
	b, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(b) < 24 {
		return "unavailable"
	}
	scale := float64(binary.LittleEndian.Uint64(b[16:24]))
	l := func(i int) float64 { return float64(binary.LittleEndian.Uint32(b[4*i:])) / scale }
	return fmt.Sprintf("%.2f %.2f %.2f", l(0), l(1), l(2))
}
