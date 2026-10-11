//go:build unix

package main

import "syscall"

// extraNofile is how far a run raises the soft RLIMIT_NOFILE it passes to its command: a grant
// adds up to 513 descriptors (PROTOCOL.md section 6).
const extraNofile = 256

// raiseChildNofile sets the soft RLIMIT_NOFILE the command inherits to inherited + 256, within
// the hard limit (and, on macOS, within kern.maxfilesperproc, above which setrlimit fails), and
// returns the soft limit set.
//
// It must go through syscall.Setrlimit: the Go runtime raises its own soft limit at start-up and
// restores the original one in every child it starts, unless the program sets the limit itself
// through that function. Go keeps the original private, so the caller passes the soft limit it
// observes, which is at least the original one.
func raiseChildNofile(inherited int) (soft uint64, err error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, err
	}
	soft = min(uint64(max(inherited, 0))+extraNofile, uint64(lim.Max))
	if c := fileCap(); c > 0 {
		soft = min(soft, c)
	}
	setLimit(&lim.Cur, soft)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, err
	}
	return soft, nil
}

// currentNofile is the soft RLIMIT_NOFILE this process observes.
func currentNofile() int {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0
	}
	return int(min(lim.Cur, 1<<30))
}

// setLimit stores v in an rlimit field, which is uint64 on some systems and int64 on others.
func setLimit[T int64 | uint64](field *T, v uint64) { *field = T(v) }
