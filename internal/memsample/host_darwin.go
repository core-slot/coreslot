//go:build darwin

package memsample

import (
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// vmVolume holds the swap files; swap grows into its free space.
const vmVolume = "/System/Volumes/VM"

// readHost reads the paging counters of host_statistics64, vm.swapusage, the free space of the
// VM volume and the two clocks.
func readHost() (HostReading, error) {
	var r HostReading
	var err error
	var compressed uint64
	if r.Decomp, r.SwapIns, compressed, err = vmCounters(); err != nil {
		return r, err
	}
	page, ok := sysctlNum("vm.pagesize")
	if !ok {
		return r, errors.New("sysctl vm.pagesize: cannot be read")
	}
	r.CompressBytes = compressed * page
	// struct xsw_usage: xsu_total, xsu_avail and xsu_used in bytes, then the page size.
	b, err := unix.SysctlRaw("vm.swapusage")
	if err != nil || len(b) < 24 {
		return r, fmt.Errorf("sysctl vm.swapusage: %d bytes, %v", len(b), err)
	}
	r.SwapFreeMiB = int64(binary.LittleEndian.Uint64(b[8:]) >> 20)
	r.SwapUsedMiB = int64(binary.LittleEndian.Uint64(b[16:]) >> 20)
	var fs unix.Statfs_t
	if err := unix.Statfs(vmVolume, &fs); err != nil {
		return r, fmt.Errorf("statfs %s: %w", vmVolume, err)
	}
	r.VMVolumeFreeMiB = int64(fs.Bavail * uint64(fs.Bsize) >> 20)
	r.WallNS, r.AwakeNS, err = clocks()
	return r, err
}
