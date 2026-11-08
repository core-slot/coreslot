//go:build darwin

package memsample

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// clocks reads CLOCK_MONOTONIC_RAW, which counts sleep, and CLOCK_UPTIME_RAW, which does not.
// The awake clock is read first, so that on a machine that never slept wall is not behind it.
func clocks() (wall, awake int64, err error) {
	var w, a unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_UPTIME_RAW, &a); err != nil {
		return 0, 0, fmt.Errorf("clock_gettime: %w", err)
	}
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, &w); err != nil {
		return 0, 0, fmt.Errorf("clock_gettime: %w", err)
	}
	return w.Nano(), a.Nano(), nil
}
