//go:build linux

package memsample

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Tree returns, in KiB, RssAnon + VmSwap summed over root and its descendants, plus the largest
// RssShmem among them once: shared memory is mapped by several of them but held once. It also
// returns their pids.
func Tree(root int) (kib int64, times map[int]TreeTimes, ok bool) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0, nil, false
	}
	parents := make(map[int]int, len(ents))
	stats := make(map[int][]string, len(ents))
	for _, ent := range ents {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		if f, ok := stat(pid); ok && len(f) >= 13 {
			if ppid, err := strconv.Atoi(f[1]); err == nil {
				parents[pid], stats[pid] = ppid, f
			}
		}
	}
	const tickNS = int64(time.Second) / userHZ
	var shmem int64
	kib, times, ok = treeSum(root, parents, func(pid int) (int64, TreeTimes, bool) {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
		if err != nil {
			return 0, TreeTimes{}, false
		}
		anon, ok := meminfo(string(data), "RssAnon")
		user, err1 := strconv.ParseInt(stats[pid][11], 10, 64)
		sys, err2 := strconv.ParseInt(stats[pid][12], 10, 64)
		if !ok || err1 != nil || err2 != nil {
			return 0, TreeTimes{}, false
		}
		swap, _ := meminfo(string(data), "VmSwap")
		shm, _ := meminfo(string(data), "RssShmem")
		shmem = max(shmem, shm)
		return anon + swap, TreeTimes{UserNS: user * tickNS, SysNS: sys * tickNS}, true
	})
	return kib + shmem, times, ok
}

// ContainerClient reports whether root or a descendant is named by ContainerProgram, by its
// /proc/PID/comm. A process that cannot be read is skipped.
func ContainerClient(root int) bool {
	procs, ok := Listing()
	if !ok {
		return false
	}
	parents := make(map[int]int, len(procs))
	for pid, p := range procs {
		parents[pid] = p.PPID
	}
	for _, pid := range descendants(root, parents) {
		if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm"); err == nil && ContainerProgram(commName(string(data))) {
			return true
		}
	}
	return false
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
