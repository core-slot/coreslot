//go:build darwin

package memsample

import (
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// pageWait reports whether this kernel's rusage_info_v6 carries ri_page_wait_time_mach: Darwin 27
// and later. Earlier kernels that know V6 leave that word reserved.
var pageWait = sync.OnceValue(func() bool {
	release, err := unix.Sysctl("kern.osrelease")
	major, _, _ := strings.Cut(release, ".")
	n, err2 := strconv.Atoi(major)
	return err == nil && err2 == nil && n >= 27
})

// ReadTreeTimes adds the user, system and page-wait time of pids from proc_pid_rusage: V6 where
// it carries page wait, V4 elsewhere.
func ReadTreeTimes(pids []int) (TreeTimes, error) {
	tb, err := timebase()
	if err != nil {
		return TreeTimes{}, err
	}
	ns := func(ticks uint64) int64 { return int64(ticks * uint64(tb[0]) / uint64(tb[1])) }
	has := pageWait()
	flavor := rusageV4
	if has {
		flavor = rusageV6
	}
	return treeTimes(pids, func(pid int) (TreeTimes, bool) {
		ri, ok := rusage(pid, flavor)
		t := TreeTimes{UserNS: ns(ri[userAt]), SysNS: ns(ri[sysAt]), HasPageWait: has}
		if has {
			t.PageWaitNS = ns(ri[pageWaitAt])
		}
		return t, ok
	})
}
