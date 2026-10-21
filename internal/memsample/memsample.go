// Package memsample reads the machine's memory.
package memsample

import (
	"strconv"
	"strings"
)

// meminfo returns the value of key, in KiB, from the text of /proc/meminfo. It lives here, not
// in the Linux file, so that it is tested on every platform.
func meminfo(text, key string) (kib int64, ok bool) {
	for line := range strings.Lines(text) {
		name, rest, found := strings.Cut(line, ":")
		if !found || name != key {
			continue
		}
		f := strings.Fields(rest)
		if len(f) != 2 || f[1] != "kB" {
			return 0, false
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		return n, err == nil && n >= 0
	}
	return 0, false
}
