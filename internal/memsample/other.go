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

// ReadTreeTimes is implemented on macOS and Linux only; elsewhere it is an error.
func ReadTreeTimes(pids []int) (TreeTimes, error) {
	return TreeTimes{}, fmt.Errorf("process times are not implemented on %s", runtime.GOOS)
}

// Source is empty: Tree measures nothing here.
const Source = ""

// Tree is implemented on macOS and Linux only; elsewhere it reads nothing.
func Tree(root int) (kib int64, ok bool) { return 0, false }

// Headroom is implemented on macOS and Linux only; elsewhere it reads nothing.
func Headroom() (mib int64, ok bool) { return 0, false }

// Listing is implemented on macOS and Linux only; elsewhere it reads nothing.
func Listing() (map[int]Proc, bool) { return nil, false }

// resident is implemented on macOS and Linux only; elsewhere it reads nothing.
func resident(pid int) (private, shared int64, ok bool) { return 0, 0, false }
