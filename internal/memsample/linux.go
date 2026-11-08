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
		if _, f, ok := stat(pid); ok && len(f) >= 13 {
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

// userHZ is the unit of the start time in /proc/<pid>/stat: clock ticks of 1/100 s.
const userHZ = 100

// Listing reads the process table once: every process's parent, start time and name from /proc,
// the start time as its ticks after boot plus the boot time of /proc/stat.
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
		name, f, ok := stat(pid)
		if !ok || len(f) < 20 {
			continue
		}
		ppid, err1 := strconv.Atoi(f[1])
		ticks, err2 := strconv.ParseInt(f[19], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		procs[pid] = Proc{PPID: ppid, StartNS: btime*int64(time.Second) + ticks*(int64(time.Second)/userHZ), Name: name}
	}
	return procs, true
}

// stat returns the command name of /proc/<pid>/stat, between the first '(' and the last ')' (a name
// may hold either, and spaces), and the fields after it: f[0] is the state (field 3), f[1] the
// parent pid, f[19] the start time in ticks (field 22).
func stat(pid int) (name string, f []string, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", nil, false
	}
	return parseStat(string(data))
}

// parseStat is stat's reading of one line.
func parseStat(s string) (name string, f []string, ok bool) {
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return "", nil, false
	}
	_, name, _ = strings.Cut(s[:end], "(")
	return name, strings.Fields(s[end+1:]), true
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

// Parts reads the machine's memory by part from /proc/meminfo, /proc/vmstat and, where the kernel
// reports it, /proc/pressure/memory; a Linux kernel has no pressure level, so it is 0.
func Parts() (parts map[string]int64, pressure int, ok bool) {
	mi, err := os.ReadFile(procMeminfo)
	if err != nil {
		return nil, 0, false
	}
	vm, _ := os.ReadFile("/proc/vmstat")
	var psi []byte
	if hasPSI() {
		psi, _ = os.ReadFile(procPSI)
	}
	parts, ok = linuxParts(string(mi), string(vm), string(psi))
	return parts, 0, ok
}
