//go:build unix

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// StateError is a state error (PROTOCOL.md section 14): an unusable state directory, a refused
// filesystem, an entry of an unexpected type, owner or mode, a full directory, a malformed
// FORMAT, or an inode mismatch at a held token. The caller decides whether to fail open.
type StateError struct {
	Reason string
}

func (e *StateError) Error() string { return "coreslot state error: " + e.Reason }

func stateErrorf(format string, args ...any) *StateError {
	return &StateError{Reason: fmt.Sprintf(format, args...)}
}

// Modes of everything the store creates (PROTOCOL.md section 4.2).
const (
	dirMode  = 0o700
	fileMode = 0o600
)

// CheckEntry applies the section 4.2 policy to one path: it must be a directory (wantDir) or a
// regular file, not a symlink, owned by the process's user and not group- or world-writable.
// An absent path passes: the policy applies to existing entries. Under the state-directory
// override the engine calls it on <statedir>/config.toml before loading the configuration.
func CheckEntry(path string, wantDir bool) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return stateErrorf("%s: %v", path, err)
	}
	return checkInfo(path, fi, wantDir)
}

func checkInfo(path string, fi fs.FileInfo, wantDir bool) error {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return stateErrorf("%s is a symlink", path)
	case wantDir && !fi.IsDir():
		return stateErrorf("%s is not a directory (%v)", path, fi.Mode().Type())
	case !wantDir && !fi.Mode().IsRegular():
		return stateErrorf("%s is not a regular file (%v)", path, fi.Mode().Type())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return stateErrorf("%s: no ownership information", path)
	}
	return checkOwner(path, st.Uid, uint32(st.Mode))
}

func checkOwner(path string, uid, mode uint32) error {
	if me := os.Getuid(); int(uid) != me {
		return stateErrorf("%s is owned by uid %d, not by uid %d", path, uid, me)
	}
	if mode&0o022 != 0 {
		return stateErrorf("%s is group- or world-writable (mode %04o)", path, mode&0o7777)
	}
	return nil
}

// OpenFile opens a file under the state directory with O_NOFOLLOW and O_CLOEXEC added, creating
// it with mode 0600 when flag has O_CREATE, and checks the open file as section 4.2 requires
// (regular, owned by the user, not group- or world-writable). It never blocks on a FIFO: the
// open is non-blocking and blocking mode is restored once the file is known to be regular.
func OpenFile(path string, flag int) (*os.File, error) {
	fd, err := unix.Open(path, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, fileMode)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENXIO) || errors.Is(err, unix.EISDIR) {
			if fi, lerr := os.Lstat(path); lerr == nil {
				if cerr := checkInfo(path, fi, false); cerr != nil {
					return nil, cerr
				}
			}
		}
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, &fs.PathError{Op: "fstat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, stateErrorf("%s is not a regular file (mode %o)", path, st.Mode&unix.S_IFMT)
	}
	if err := checkOwner(path, st.Uid, uint32(st.Mode)); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return nil, &fs.PathError{Op: "fcntl", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// refusedType reports whether a macOS filesystem type name (statfs f_fstypename) is a network,
// FUSE or VM-shared filesystem the protocol refuses (PROTOCOL.md section 4.3).
func refusedType(name string) bool {
	switch name {
	case "nfs", "smbfs", "afpfs", "webdav":
		return true
	}
	return strings.HasPrefix(name, "macfuse") || strings.HasPrefix(name, "osxfuse")
}

// refusedMagic reports whether a Linux filesystem magic (statfs f_type) is a network, FUSE or
// VM-shared filesystem the protocol refuses (PROTOCOL.md section 4.3): NFS, CIFS, SMB2, SMB, FUSE,
// 9p, the VirtualBox shared folder, Ceph, AFS and kAFS.
func refusedMagic(m int64) bool {
	switch m {
	case 0x6969, // NFS
		0xFF534D42, // CIFS
		0xFE534D42, // SMB2
		0x517B,     // SMB
		0x65735546, // FUSE
		0x01021997, // 9p
		0x786F4256, // vboxsf
		0x00C36400, // Ceph
		0x5346414F, // AFS (OpenAFS, kAFS superblock)
		0x6B414653: // kAFS ("kAFS")
		return true
	}
	return false
}
