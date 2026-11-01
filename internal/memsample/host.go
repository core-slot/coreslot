package memsample

import (
	"errors"
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
	Decomp, SwapIns                           uint64
	PSISomeUS                                 uint64
	HasPSI                                    bool
	SwapUsedMiB, SwapFreeMiB, VMVolumeFreeMiB int64
	// WallNS counts the time the machine slept and AwakeNS does not; both are in ns since boot.
	WallNS, AwakeNS int64
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

const (
	gapSkewNS = int64(time.Second)
	gapSpanNS = int64(6 * time.Second)
)

// Gap reports whether prev and cur are too far apart to compare: the two clocks diverged by more
// than 1 s (the machine slept), more than 6 s passed, or the clock went back (a reboot).
func Gap(prev, cur HostReading) bool {
	wall, awake := cur.WallNS-prev.WallNS, cur.AwakeNS-prev.AwakeNS
	return wall < 0 || wall > gapSpanNS || max(wall-awake, awake-wall) > gapSkewNS
}

// treeTimes adds read over pids. A process that cannot be read (it exited) is skipped; it is an
// error when not one of them could be read.
func treeTimes(pids []int, read func(pid int) (TreeTimes, bool)) (TreeTimes, error) {
	var sum TreeTimes
	found := false
	for _, pid := range pids {
		if t, ok := read(pid); ok {
			sum.UserNS += t.UserNS
			sum.SysNS += t.SysNS
			sum.PageWaitNS += t.PageWaitNS
			sum.HasPageWait = t.HasPageWait
			found = true
		}
	}
	if !found {
		return sum, errors.New("no process of the tree could be read")
	}
	return sum, nil
}

// psiTotal returns the total= field of the "some" line of /proc/pressure/memory's text, in µs.
// It lives here, not in the Linux file, so that it is tested on every platform.
func psiTotal(text string) (us uint64, ok bool) {
	for line := range strings.Lines(text) {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "some" {
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
