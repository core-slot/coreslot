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

// HeadroomParts are the page counts a macOS headroom reading comes from, in MiB: free,
// speculative, purgeable, file-backed (external) and anonymous (internal) pageable pages. Linux
// reads none.
type HeadroomParts struct {
	FreeMiB, SpeculativeMiB, PurgeableMiB, FileBackedMiB, InternalMiB int64
}

// The headroom modes of macOS (HeadroomParts.Headroom), as the sample log names them.
const (
	HeadroomStrict = "strict"
	HeadroomCheap  = "cheap"
)

// fileCacheKeptMiB is the file cache the macOS kernel keeps under memory pressure: below about
// this much it compresses applications' memory instead of releasing file-backed pages, so that
// much of the file-backed count is never handed out without paging. Measured on one 16 GB Mac
// (about 1.6 to 1.7 GiB); larger Macs are not yet measured, and it may scale with their memory.
const fileCacheKeptMiB = 1792

// idleAnonShare is the share of the anonymous memory outside coreslot's jobs that the cheap
// reading counts as room. Under pressure the kernel compresses idle applications' memory, and the
// compressor frees 45 to 56 % of what it takes: in measurements on one 16 GB Mac, 47 to 55 % of
// that memory became room before any job slowed. 0.4 leaves a margin under the lowest.
const idleAnonShare = 0.4

// CompressHarmBytesS is the compression rate, compressions and swap-outs in bytes/s, from which
// the macOS headroom is the strict reading. In the same measurements, compressing idle memory ran
// at 0.05 to 0.8 GiB/s while no job slowed, and at 1.5 GiB/s and more while jobs did.
const CompressHarmBytesS = 1 << 30

// Strict is the strict macOS reading of the parts, in MiB: free and purgeable pages and the
// file-backed pages above the cache the kernel keeps, never below 0. These functions live here,
// not in the macOS file, so that they are tested on every platform.
func (p HeadroomParts) Strict() int64 {
	return max(p.FreeMiB+p.PurgeableMiB+max(p.FileBackedMiB-fileCacheKeptMiB, 0), 0)
}

// Cheap is the cheap macOS reading of the parts, in MiB: the strict one plus idleAnonShare of the
// anonymous pages (internal less purgeable) less footprintMiB, what coreslot's own live jobs hold,
// which becomes no room. A stop round's shortage always reads it.
func (p HeadroomParts) Cheap(footprintMiB int64) int64 {
	anon := max(p.InternalMiB-p.PurgeableMiB-max(footprintMiB, 0), 0)
	return p.Strict() + int64(idleAnonShare*float64(anon))
}

// Headroom returns the macOS headroom admission reads, in MiB, and its mode: Strict when
// compressBytesS, the compression rate of the last period, is at least CompressHarmBytesS, and
// when the rate is unknown (nil) while holdsLive; Cheap(footprintMiB) otherwise.
func (p HeadroomParts) Headroom(footprintMiB int64, compressBytesS *float64, holdsLive bool) (mib int64, mode string) {
	if compressBytesS == nil && holdsLive || compressBytesS != nil && *compressBytesS >= CompressHarmBytesS {
		return p.Strict(), HeadroomStrict
	}
	return p.Cheap(footprintMiB), HeadroomCheap
}

// darwinParts are the headroom parts of the page counts, each times pageSize, in MiB. It lives
// here, not in the macOS file, so that it is tested on every platform.
func darwinParts(free, speculative, purgeable, fileBacked, internal, pageSize uint64) HeadroomParts {
	mib := func(pages uint64) int64 { return int64(pages * pageSize >> 20) }
	return HeadroomParts{FreeMiB: mib(free), SpeculativeMiB: mib(speculative), PurgeableMiB: mib(purgeable),
		FileBackedMiB: mib(fileBacked), InternalMiB: mib(internal)}
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
