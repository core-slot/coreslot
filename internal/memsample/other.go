//go:build !darwin && !linux

package memsample

import (
	"fmt"
	"runtime"
)

// readHost is implemented on macOS and Linux only; elsewhere it is an error.
func readHost() (HostReading, error) {
	return HostReading{}, fmt.Errorf("the machine's readings are not implemented on %s", runtime.GOOS)
}

// Tree is implemented on macOS and Linux only; elsewhere it reads nothing.
func Tree(root int) (kib int64, times map[int]TreeTimes, ok bool) { return 0, nil, false }

// Headroom is implemented on macOS and Linux only; elsewhere it reads nothing.
func Headroom() (mib int64, parts *HeadroomParts, ok bool) { return 0, nil, false }

// Listing is implemented on macOS and Linux only; elsewhere it reads nothing.
func Listing() (map[int]Proc, bool) { return nil, false }
