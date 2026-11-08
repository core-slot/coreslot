//go:build darwin || linux

package memsample

// Clocks returns the clock that counts sleep and the one that does not, both in nanoseconds on
// the machine's boot clocks; 0, 0 when they cannot be read.
func Clocks() (upNS, awakeNS int64) {
	up, awake, err := clocks()
	if err != nil {
		return 0, 0
	}
	return up, awake
}
