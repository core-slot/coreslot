//go:build linux

package memsample

import (
	"fmt"
	"os"
)

const procMeminfo = "/proc/meminfo"

// Total returns the machine's physical memory in MiB: MemTotal of /proc/meminfo. It is never a
// cgroup's limit.
func Total() (int64, error) {
	data, err := os.ReadFile(procMeminfo)
	if err != nil {
		return 0, err
	}
	kib, ok := meminfo(string(data), "MemTotal")
	if !ok {
		return 0, fmt.Errorf("%s: no MemTotal in kB", procMeminfo)
	}
	return kib / 1024, nil
}
