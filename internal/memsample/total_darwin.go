//go:build darwin

package memsample

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Total returns the machine's physical memory in MiB: the hw.memsize sysctl.
func Total() (int64, error) {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	return int64(n >> 20), nil
}
