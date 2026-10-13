//go:build darwin

package config

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// MachineCPUs returns the machine-wide online CPU count: the hw.logicalcpu sysctl. It is never a
// process's affinity or quota.
func MachineCPUs() (int, error) {
	n, err := unix.SysctlUint32("hw.logicalcpu")
	if err != nil {
		return 0, fmt.Errorf("sysctl hw.logicalcpu: %w", err)
	}
	if n < 1 {
		return 0, fmt.Errorf("sysctl hw.logicalcpu is %d", n)
	}
	return int(n), nil
}
