//go:build !darwin && !linux

package memsample

import (
	"fmt"
	"runtime"
)

// Total is implemented on macOS and Linux only; elsewhere it is an error.
func Total() (int64, error) {
	return 0, fmt.Errorf("the machine's memory is not implemented on %s", runtime.GOOS)
}
