//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// CORESLOT_HELD_FD is parsed with the strict descriptor grammar (PROTOCOL.md section 17): a value
// such as "+N" or " N" names no descriptor, so dropping an inherited context closes nothing for
// it; a plain "N" names N, a regular file, which is closed.
func TestDropInheritedParsesHeldFDStrictly(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "held"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	open := func() int {
		fd, err := unix.Dup(int(f.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}
	isOpen := func(fd int) bool {
		var st unix.Stat_t
		return unix.Fstat(fd, &st) == nil
	}
	for _, affix := range [][2]string{{"+", ""}, {" ", ""}, {"", " "}} {
		fd := open()
		text := affix[0] + strconv.Itoa(fd) + affix[1]
		dropInherited(map[string]string{"CORESLOT_HELD_FD": text})
		if !isOpen(fd) {
			t.Errorf("CORESLOT_HELD_FD=%q closed descriptor %d", text, fd)
		} else {
			unix.Close(fd)
		}
	}
	fd := open()
	dropInherited(map[string]string{"CORESLOT_HELD_FD": strconv.Itoa(fd)})
	if isOpen(fd) {
		unix.Close(fd)
		t.Errorf("CORESLOT_HELD_FD=%d left the descriptor open", fd)
	}
}
