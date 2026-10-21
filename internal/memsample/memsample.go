// Package memsample reads the machine's memory: its total, the kernel's headroom, and the private
// memory a process tree holds. Tree's measure is Source: on macOS each process's physical
// footprint, on Linux each process's anonymous resident memory and swap, plus the tree's largest
// shared memory once. HolderUse measures the same without swap, which is not resident.
package memsample

import (
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Proc is one process of a listing: its parent and its start time, in ns since the epoch.
type Proc struct {
	PPID    int
	StartNS int64
}

// startSlack is how much later than its hold's start a holder may have started: the listing's
// start time and the hold's clock read differ by the kernel's rounding.
const startSlack = int64(time.Second)

// HolderUse returns, per root pid, the resident memory in KiB of its tree in procs, leaving out
// the subtree of every other root. A root absent from procs, started more than 1 s after
// roots[pid] (its hold's start, ns), or none of whose processes could be read, is absent.
func HolderUse(procs map[int]Proc, roots map[int]int64) map[int]int64 {
	return holderUse(procs, roots, resident)
}

// holderUse is HolderUse over read, which gives a process's private and shared KiB: private adds
// over the tree, the largest shared counts once.
func holderUse(procs map[int]Proc, roots map[int]int64, read func(pid int) (private, shared int64, ok bool)) map[int]int64 {
	parents := make(map[int]int, len(procs))
	for pid, p := range procs {
		parents[pid] = p.PPID
	}
	stop := make(map[int]bool, len(roots))
	for pid := range roots {
		stop[pid] = true
	}
	use := make(map[int]int64, len(roots))
	for root, start := range roots {
		if p, ok := procs[root]; !ok || p.StartNS > start+startSlack {
			continue
		}
		var sum, shmem int64
		found := false
		for _, pid := range descendants(root, parents, stop) {
			if private, shared, ok := read(pid); ok {
				sum += private
				shmem = max(shmem, shared)
				found = true
			}
		}
		if found {
			use[root] = sum + shmem
		}
	}
	return use
}

// descendants returns root and every process below it in parents (pid to parent pid), sorted,
// leaving out the subtree of every pid in stop other than root.
func descendants(root int, parents map[int]int, stop map[int]bool) []int {
	children := make(map[int][]int)
	for pid, ppid := range parents {
		if pid != ppid {
			children[ppid] = append(children[ppid], pid)
		}
	}
	seen := map[int]bool{root: true}
	out := []int{root}
	for i := 0; i < len(out); i++ {
		for _, c := range children[out[i]] {
			if !seen[c] && !stop[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	slices.Sort(out)
	return out
}

// treeSum adds read over root and its descendants in parents. A process that cannot be read
// (it exited after the listing) is skipped; ok is false when not one of them could be read.
func treeSum(root int, parents map[int]int, read func(pid int) (kib int64, ok bool)) (int64, bool) {
	var sum int64
	found := false
	for _, pid := range descendants(root, parents, nil) {
		if kib, ok := read(pid); ok {
			sum += kib
			found = true
		}
	}
	return sum, found
}

// meminfo returns the value of key, in KiB, from the text of /proc/meminfo. It lives here, not
// in the Linux file, so that it is tested on every platform.
func meminfo(text, key string) (kib int64, ok bool) {
	for line := range strings.Lines(text) {
		name, rest, found := strings.Cut(line, ":")
		if !found || name != key {
			continue
		}
		f := strings.Fields(rest)
		if len(f) != 2 || f[1] != "kB" {
			return 0, false
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		return n, err == nil && n >= 0
	}
	return 0, false
}

// cgroupLimit returns the room, in MiB, under the tightest memory.max of the cgroup v2 named by
// the 0:: line of selfCgroup (the text of /proc/self/cgroup) and its ancestors below
// /sys/fs/cgroup: memory.max less memory.current, never below 0. A level whose memory.max is
// "max" or that cannot be read is skipped; ok is false when no level sets a limit, or when there
// is no 0:: line (cgroup v1). It lives here, not in the Linux file, so that it is tested on every
// platform.
func cgroupLimit(selfCgroup string, read func(path string) (string, error)) (mib int64, ok bool) {
	var cg string
	found := false
	for line := range strings.Lines(selfCgroup) {
		if cg, found = strings.CutPrefix(strings.TrimSpace(line), "0::"); found {
			break
		}
	}
	if !found {
		return 0, false
	}
	num := func(dir, name string) (int64, bool) {
		v, err := read(path.Join("/sys/fs/cgroup", dir, name))
		if err != nil {
			return 0, false
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	}
	for dir := cg; ; dir = path.Dir(dir) {
		limit, ok1 := num(dir, "memory.max")
		current, ok2 := num(dir, "memory.current")
		if ok1 && ok2 {
			room := max(limit-current, 0) >> 20
			if !ok || room < mib {
				mib, ok = room, true
			}
		}
		if parent := path.Dir(dir); parent == "/" || parent == "." || parent == dir {
			break
		}
	}
	return mib, ok
}
