// Package memsample reads the machine's memory: its total, the kernel's headroom, and the private
// memory a process tree holds. Tree's measure is Source: on macOS each process's physical
// footprint, on Linux each process's anonymous resident memory and swap, plus the tree's largest
// shared memory once.
package memsample

import (
	"slices"
	"strconv"
	"strings"
)

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

// treeSum adds read over root and its descendants in parents. A process that cannot be read
// (it exited after the listing) is skipped; ok is false when not one of them could be read.
func treeSum(root int, parents map[int]int, read func(pid int) (kib int64, ok bool)) (int64, bool) {
	var sum int64
	found := false
	for _, pid := range descendants(root, parents) {
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
