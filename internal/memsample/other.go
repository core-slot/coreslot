//go:build !darwin && !linux

package memsample

// Source is empty: Tree measures nothing here.
const Source = ""

// Tree is implemented on macOS and Linux only; elsewhere it reads nothing.
func Tree(root int) (kib int64, ok bool) { return 0, false }

// Headroom is implemented on macOS and Linux only; elsewhere it reads nothing.
func Headroom() (mib int64, ok bool) { return 0, false }
