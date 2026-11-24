//go:build !darwin

package memsample

// lookupVM reads nothing off macOS: Docker's VM is never found. A test replaces it.
var lookupVM = func() VM { return VM{State: VMNotRunning} }

// vmFootprint reads nothing off macOS.
func vmFootprint(int, int64) (int64, bool) { return 0, false }
