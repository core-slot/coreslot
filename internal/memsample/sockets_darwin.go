//go:build darwin

package memsample

import (
	"encoding/binary"
	"slices"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The proc_info calls and the layout of struct socket_fdinfo read, from sys/proc_info.h of the
// macOS SDK. Its fields are fixed-width and clang lays them out the same on arm64 and x86_64:
// offsetof(struct socket_fdinfo, psi.soi_kind) is 256 and psi.soi_proto.pri_un.unsi_caddr 535, and
// sizeof is 792.
const (
	callPidInfo   = 2 // PROC_INFO_CALL_PIDINFO
	callPidFdInfo = 3 // PROC_INFO_CALL_PIDFDINFO
	pidListFds    = 1 // PROC_PIDLISTFDS: an array of struct proc_fdinfo {int32 fd; uint32 type}
	pidFdSocket   = 3 // PROC_PIDFDSOCKETINFO
	fdTypeSocket  = 2 // PROX_FDTYPE_SOCKET

	sockFdInfoSize = 792 // sizeof(struct socket_fdinfo)
	kindAt         = 256 // psi.soi_kind, an int32
	caddrAt        = 535 // psi.soi_proto.pri_un.unsi_caddr, a struct sockaddr_un
	sockinfoUn     = 3   // SOCKINFO_UN
	sunPathAt      = 2   // offsetof(struct sockaddr_un, sun_path)
	sunPathLen     = 104 // sizeof(sun_path)
)

// info is proc_info(2): call, flavor and arg select what is read into b. An empty b is passed as
// a nil pointer, which asks how many bytes the answer needs.
func info(call, pid, flavor, arg int, b []byte) (int, error) {
	var p unsafe.Pointer
	if len(b) > 0 {
		p = unsafe.Pointer(&b[0])
	}
	//lint:ignore SA1019 x/sys/unix wraps no proc_info; a libSystem call needs cgo or an assembly trampoline
	r, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, uintptr(call), uintptr(pid), uintptr(flavor),
		uintptr(arg), uintptr(p), uintptr(len(b)))
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

// peers returns the bound paths that pid's unix sockets are connected to. A process that is gone,
// or that the caller may not read, has none. A descriptor opened after the sizing call may be
// missed: the scan comes round again in a second.
func peers(pid int) []string {
	n, err := info(callPidInfo, pid, pidListFds, 0, nil)
	if err != nil || n <= 0 {
		return nil
	}
	b := make([]byte, n)
	if n, err = info(callPidInfo, pid, pidListFds, 0, b); err != nil {
		return nil
	}
	var out []string
	s := make([]byte, sockFdInfoSize)
	for i := 0; i+8 <= n; i += 8 {
		fd, typ := int(int32(binary.LittleEndian.Uint32(b[i:]))), binary.LittleEndian.Uint32(b[i+4:])
		if typ != fdTypeSocket {
			continue
		}
		if m, err := info(callPidFdInfo, pid, pidFdSocket, fd, s); err != nil || m < len(s) {
			continue
		}
		if p := peerPath(s); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// peerPath is the path a socket_fdinfo's unix socket is connected to, empty for any other kind, whose
// bytes at that offset are no path.
func peerPath(s []byte) string {
	if binary.LittleEndian.Uint32(s[kindAt:]) != sockinfoUn {
		return ""
	}
	return unix.ByteSliceToString(s[caddrAt+sunPathAt : caddrAt+sunPathAt+sunPathLen])
}

// holdsDockerSocket reports whether pid holds a unix socket connected to one containerSocket accepts.
func holdsDockerSocket(pid int) bool { return slices.ContainsFunc(peers(pid), containerSocket) }
