//go:build !darwin

package memsample

// holdsDockerSocket is implemented on macOS only: elsewhere a client's socket names no peer.
func holdsDockerSocket(int) bool { return false }
