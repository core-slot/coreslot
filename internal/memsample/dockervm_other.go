//go:build !darwin

package memsample

// lookupVM reads nothing off macOS: Docker's VM is never found.
func lookupVM() VM { return VM{State: VMNotRunning} }
