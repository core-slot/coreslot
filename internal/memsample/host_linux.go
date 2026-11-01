//go:build linux

package memsample

import (
	"errors"
	"os"
	"sync"
)

const procPSI = "/proc/pressure/memory"

// hasPSI reports whether the kernel reports memory pressure: a kernel without PSI has no such
// file, and one booted with psi=0 fails the read with EOPNOTSUPP.
var hasPSI = sync.OnceValue(func() bool {
	data, err := os.ReadFile(procPSI)
	_, ok := psiTotal(string(data))
	return err == nil && ok
})

// readHost reads pswpin of /proc/vmstat, the swap of /proc/meminfo, the PSI memory stall where
// the kernel reports it, and the two clocks.
func readHost() (HostReading, error) {
	var r HostReading
	data, err := os.ReadFile("/proc/vmstat")
	if err != nil {
		return r, err
	}
	var ok bool
	if r.SwapIns, ok = vmstat(string(data), "pswpin"); !ok {
		return r, errors.New("/proc/vmstat: no pswpin")
	}
	if data, err = os.ReadFile(procMeminfo); err != nil {
		return r, err
	}
	total, ok1 := meminfo(string(data), "SwapTotal")
	free, ok2 := meminfo(string(data), "SwapFree")
	if !ok1 || !ok2 {
		return r, errors.New(procMeminfo + ": no SwapTotal or SwapFree in kB")
	}
	r.SwapFreeMiB, r.SwapUsedMiB = free/1024, (total-free)/1024
	if hasPSI() {
		if data, err := os.ReadFile(procPSI); err == nil {
			r.PSISomeUS, r.HasPSI = psiTotal(string(data))
		}
	}
	r.WallNS, r.AwakeNS, err = clocks()
	return r, err
}
