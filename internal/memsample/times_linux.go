//go:build linux

package memsample

import (
	"strconv"
	"time"
)

// ReadTreeTimes adds the utime and stime of /proc/<pid>/stat over pids, in clock ticks of 1/100
// s. Linux reports no page wait.
func ReadTreeTimes(pids []int) (TreeTimes, error) {
	const tickNS = int64(time.Second) / userHZ
	return treeTimes(pids, func(pid int) (TreeTimes, bool) {
		f, ok := stat(pid)
		if !ok || len(f) < 13 {
			return TreeTimes{}, false
		}
		user, err1 := strconv.ParseInt(f[11], 10, 64)
		sys, err2 := strconv.ParseInt(f[12], 10, 64)
		return TreeTimes{UserNS: user * tickNS, SysNS: sys * tickNS}, err1 == nil && err2 == nil
	})
}
