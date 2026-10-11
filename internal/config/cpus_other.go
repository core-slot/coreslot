//go:build !darwin && !linux

package config

import (
	"fmt"
	"runtime"
)

// MachineCPUs is implemented on macOS and Linux only; elsewhere it is an error, which callers
// treat as a state error.
func MachineCPUs() (int, error) {
	return 0, fmt.Errorf("the machine-wide CPU count is not implemented on %s", runtime.GOOS)
}
