package memsample

import (
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/core-slot/coreslot/internal/testhooks"
)

// HostReading is one reading of the machine's paging counters, swap room and clocks. The
// counters are cumulative since boot: pages decompressed (macOS) and swapped in, and on Linux the
// µs some task stalled on memory (PSI), when the kernel reports it.
type HostReading struct {
	Decomp          uint64 `json:"decomp"`
	SwapIns         uint64 `json:"swap_ins"`
	PSISomeUS       uint64 `json:"psi_some_us"`
	HasPSI          bool   `json:"has_psi"`
	SwapUsedMiB     int64  `json:"swap_used_mib"`
	SwapFreeMiB     int64  `json:"swap_free_mib"`
	VMVolumeFreeMiB int64  `json:"vm_volume_free_mib"`
	MemAvailableMiB int64  `json:"mem_available_mib"` // Linux: MemAvailable, which swap room counts there; 0 elsewhere
	// WallNS counts the time the machine slept and AwakeNS does not; both are in ns since boot.
	WallNS  int64 `json:"wall_ns"`
	AwakeNS int64 `json:"awake_ns"`
}

// ReadHost reads the machine's paging counters, swap room and clocks. In a test build the
// readings hook replaces the fields it names (by their Go names; HasPSI is 0 or 1).
func ReadHost() (HostReading, error) {
	r, err := readHost()
	if nums, set := testhooks.Readings(); set && err == nil {
		r = overlay(r, nums)
	}
	return r, err
}

func overlay(r HostReading, nums map[string]int64) HostReading {
	v := reflect.ValueOf(&r).Elem()
	for name, n := range nums {
		switch f := v.FieldByName(name); f.Kind() {
		case reflect.Uint64:
			f.SetUint(uint64(n))
		case reflect.Int64:
			f.SetInt(n)
		case reflect.Bool:
			f.SetBool(n != 0)
		default:
			panic("memsample: the readings hook names no reading " + name)
		}
	}
	return r
}

// TreeTimes is the user, system and page-wait time a set of processes has spent, in ns. Page
// wait is read where the kernel reports it (macOS 27 and later).
type TreeTimes struct {
	UserNS, SysNS, PageWaitNS int64
	HasPageWait               bool
}

// PeriodNS is the watchdog's sampling period, and StaleNS, three periods, the age past which a
// reading is none: a pair of samples further apart is a gap.
const (
	PeriodNS  = int64(2 * time.Second)
	StaleNS   = 3 * PeriodNS
	gapSkewNS = int64(time.Second)
)

// Gap reports whether prev and cur are too far apart to compare: the two clocks diverged by more
// than 1 s (the machine slept), more than 6 s passed, or the clock or a counter went back (a reboot).
func Gap(prev, cur HostReading) bool {
	wall, awake := cur.WallNS-prev.WallNS, cur.AwakeNS-prev.AwakeNS
	return wall < 0 || wall > StaleNS || max(wall-awake, awake-wall) > gapSkewNS || cur.Decomp < prev.Decomp ||
		cur.SwapIns < prev.SwapIns || prev.HasPSI && cur.HasPSI && cur.PSISomeUS < prev.PSISomeUS
}

// psiTotal returns the total= field of the line of /proc/pressure/memory's text that starts with
// kind ("some" or "full"), in µs. It lives here, not in the Linux file, so that it is tested on
// every platform.
func psiTotal(text, kind string) (us uint64, ok bool) {
	for line := range strings.Lines(text) {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != kind {
			continue
		}
		for _, kv := range f[1:] {
			if v, found := strings.CutPrefix(kv, "total="); found {
				n, err := strconv.ParseUint(v, 10, 64)
				return n, err == nil
			}
		}
	}
	return 0, false
}

// linuxParts is the machine's parts from the text of /proc/meminfo, /proc/vmstat and
// /proc/pressure/memory: the meminfo ones in MiB, oom_kill a count and psi_full_us in µs. A key
// whose reading is missing is absent; ok is false when meminfo gives none. It lives here so that
// it is tested on every platform.
func linuxParts(meminfoText, vmstatText, psiText string) (parts map[string]int64, ok bool) {
	parts = map[string]int64{}
	for _, k := range []string{"AnonPages", "Shmem", "Slab", "SReclaimable", "PageTables", "Zswap"} {
		if kib, found := meminfo(meminfoText, k); found {
			parts[k] = kib / 1024
		}
	}
	if len(parts) == 0 {
		return nil, false
	}
	if n, found := vmstat(vmstatText, "oom_kill"); found {
		parts["oom_kill"] = int64(n)
	}
	if us, found := psiTotal(psiText, "full"); found {
		parts["psi_full_us"] = int64(us)
	}
	return parts, true
}

// vmstat returns the value of key in the text of /proc/vmstat. It lives here, not in the Linux
// file, so that it is tested on every platform.
func vmstat(text, key string) (uint64, bool) {
	for line := range strings.Lines(text) {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == key {
			n, err := strconv.ParseUint(f[1], 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}
