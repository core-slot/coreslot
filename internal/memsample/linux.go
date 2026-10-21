//go:build linux

package memsample

import (
	"os"
	"strconv"
	"strings"
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

// parent reads pid's parent from /proc/<pid>/stat: the field after the last ')' is the state,
// the next one the parent pid.
func parent(pid int) (int, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(data)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(f[1])
	return ppid, err == nil
}

// Headroom returns MemAvailable of /proc/meminfo in MiB.
func Headroom() (mib int64, ok bool) {
	data, err := os.ReadFile(procMeminfo)
	if err != nil {
		return 0, false
	}
	kib, ok := meminfo(string(data), "MemAvailable")
	return kib / 1024, ok
}
