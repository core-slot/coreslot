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
