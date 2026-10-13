//go:build linux

package config

import (
	"fmt"
	"os"
)

const onlineCPUs = "/sys/devices/system/cpu/online"

// MachineCPUs returns the machine-wide online CPU count from /sys/devices/system/cpu/online. It is
// never a process's affinity mask or cgroup quota.
func MachineCPUs() (int, error) {
	data, err := os.ReadFile(onlineCPUs)
	if err != nil {
		return 0, err
	}
	n, err := parseOnline(string(data))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", onlineCPUs, err)
	}
	return n, nil
}
