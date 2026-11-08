//go:build !darwin

package memsample

// Sweep is implemented on macOS only; elsewhere it reads nothing.
func Sweep(n int) (top []ProcMem, vm *ProcMem, ok bool) { return nil, nil, false }
