// Package memsample reads the machine's memory: its total, the kernel's headroom, and the private
// memory a process tree holds. Tree's measure: on macOS each process's physical
// footprint, on Linux each process's anonymous resident memory and swap, plus the tree's largest
// shared memory once.
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

// Alive reports whether the process that started at startNS (ns, a hold's start) as pid is still
// in procs: pid is listed and started no more than 1 s after startNS. A pid that started later
// was reused by another process.
func Alive(procs map[int]Proc, pid int, startNS int64) bool {
	p, ok := procs[pid]
	return ok && p.StartNS <= startNS+startSlack
}

// descendants returns root and every process below it in parents (pid to parent pid), sorted.
func descendants(root int, parents map[int]int) []int {
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
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	slices.Sort(out)
	return out
}

// treeSum adds the memory read gives over root and its descendants in parents and returns the
// times it gave, by pid: one read per process. A process that cannot be read (it exited after the
// listing) is skipped; ok is false when not one of them could be read.
func treeSum(root int, parents map[int]int, read func(pid int) (kib int64, t TreeTimes, ok bool)) (int64, map[int]TreeTimes, bool) {
	var sum int64
	times := map[int]TreeTimes{}
	for _, pid := range descendants(root, parents) {
		if kib, t, ok := read(pid); ok {
			sum += kib
			times[pid] = t
		}
	}
	return sum, times, len(times) > 0
}

// HeadroomParts are the page counts a macOS headroom reading came from, in MiB: free, speculative,
// purgeable and file-backed (external) pageable pages. Linux reads none.
type HeadroomParts struct {
	FreeMiB, SpeculativeMiB, PurgeableMiB, FileBackedMiB int64
}

// fileCacheKeptMiB is the file cache the macOS kernel keeps under memory pressure: below about
// this much it compresses applications' memory instead of releasing file-backed pages, so that
// much of the file-backed count is never handed out without paging. Measured on one 16 GB Mac
// (about 1.6 to 1.7 GiB); it may scale with physical memory, which is not yet measured.
const fileCacheKeptMiB = 1792

// darwinHeadroom returns the macOS headroom of the page counts, in MiB, and its parts: free,
// purgeable and file-backed pages times pageSize, less the file cache the kernel keeps, never
// below 0. Speculative pages are read for the parts only: they are already file-backed. It lives
// here, not in the macOS file, so that it is tested on every platform.
func darwinHeadroom(free, speculative, purgeable, fileBacked, pageSize uint64) (int64, HeadroomParts) {
	mib := func(pages uint64) int64 { return int64(pages * pageSize >> 20) }
	parts := HeadroomParts{FreeMiB: mib(free), SpeculativeMiB: mib(speculative),
		PurgeableMiB: mib(purgeable), FileBackedMiB: mib(fileBacked)}
	return max(mib(free+purgeable+fileBacked)-fileCacheKeptMiB, 0), parts
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
