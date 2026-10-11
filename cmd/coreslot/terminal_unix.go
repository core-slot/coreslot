//go:build unix

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// terminalMode reports whether a run supervises its command in terminal mode (spec section
// 6.1): stdin is a terminal and coreslot's process group is that terminal's foreground group.
// The command then stays in coreslot's group, so the keys of the terminal reach both.
func terminalMode(stdin *os.File) bool {
	if stdin == nil {
		return false
	}
	// TIOCGPGRP fails (ENOTTY) unless stdin is a terminal: it is the isatty test too.
	pgrp, err := unix.IoctlGetInt(int(stdin.Fd()), unix.TIOCGPGRP)
	return err == nil && pgrp == unix.Getpgrp()
}
