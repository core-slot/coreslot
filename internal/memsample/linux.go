//go:build linux

package memsample

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Source names Tree's measure: anonymous resident memory and swap.
const Source = "anon"

// Tree returns, in KiB, RssAnon + VmSwap summed over root and its descendants, plus the largest
// RssShmem among them once: shared memory is mapped by several of them but held once.
func Tree(root int) (kib int64, ok bool) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	parents := make(map[int]int, len(ents))
	for _, ent := range ents {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		if ppid, ok := parent(pid); ok {
			parents[pid] = ppid
		}
	}
	var shmem int64
	kib, ok = treeSum(root, parents, func(pid int) (int64, bool) {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
		if err != nil {
			return 0, false
		}
		anon, ok := meminfo(string(data), "RssAnon")
		if !ok {
			return 0, false
		}
		swap, _ := meminfo(string(data), "VmSwap")
		shm, _ := meminfo(string(data), "RssShmem")
		shmem = max(shmem, shm)
		return anon + swap, true
	})
	return kib + shmem, ok
}

// userHZ is the unit of the start time in /proc/<pid>/stat: clock ticks of 1/100 s.
const userHZ = 100

// Listing reads the process table once: every process's parent and start time from /proc, the
// start time as its ticks after boot plus the boot time of /proc/stat.
func Listing() (map[int]Proc, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, false
	}
	var btime int64 = -1
	for line := range strings.Lines(string(data)) {
		if v, found := strings.CutPrefix(line, "btime "); found {
			btime, err = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return nil, false
			}
			break
		}
	}
	ents, err := os.ReadDir("/proc")
	if btime < 0 || err != nil {
		return nil, false
	}
	procs := make(map[int]Proc, len(ents))
	for _, ent := range ents {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		f, ok := stat(pid)
		if !ok || len(f) < 20 {
			continue
		}
		ppid, err1 := strconv.Atoi(f[1])
		ticks, err2 := strconv.ParseInt(f[19], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		procs[pid] = Proc{PPID: ppid, StartNS: btime*int64(time.Second) + ticks*(int64(time.Second)/userHZ)}
	}
	return procs, true
}

// resident reads pid's RssAnon as private and RssShmem as shared from /proc/<pid>/status. Swap
// is not resident, so it is not counted.
func resident(pid int) (private, shared int64, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, 0, false
	}
	private, ok = meminfo(string(data), "RssAnon")
	if !ok {
		return 0, 0, false
	}
	shared, _ = meminfo(string(data), "RssShmem")
	return private, shared, true
}

// parent reads pid's parent from /proc/<pid>/stat.
func parent(pid int) (int, bool) {
	f, ok := stat(pid)
	if !ok || len(f) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(f[1])
	return ppid, err == nil
}

// stat returns the fields of /proc/<pid>/stat after the last ')', which ends the command name:
// f[0] is the state (field 3), f[1] the parent pid, f[19] the start time in ticks (field 22).
func stat(pid int) ([]string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil, false
	}
	s := string(data)
	return strings.Fields(s[strings.LastIndexByte(s, ')')+1:]), true
}

// Headroom returns MemAvailable of /proc/meminfo in MiB, or the room under this process's cgroup
// v2 memory limit when that is smaller.
func Headroom() (mib int64, ok bool) {
	data, err := os.ReadFile(procMeminfo)
	if err != nil {
		return 0, false
	}
	kib, ok := meminfo(string(data), "MemAvailable")
	if !ok {
		return 0, false
	}
	mib = kib / 1024
	if self, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		if limit, ok := cgroupLimit(string(self), readString); ok {
			mib = min(mib, limit)
		}
	}
	return mib, true
}

// readString reads a file as text.
func readString(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}
