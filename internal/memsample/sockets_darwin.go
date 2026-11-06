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
// offsetof(struct socket_fdinfo, psi.soi_family) is 184, psi.soi_kind 256 and
// psi.soi_proto.pri_un.unsi_caddr 535, and sizeof is 792.
const (
	callPidInfo   = 2 // PROC_INFO_CALL_PIDINFO
	callPidFdInfo = 3 // PROC_INFO_CALL_PIDFDINFO
	pidListFds    = 1 // PROC_PIDLISTFDS: an array of struct proc_fdinfo {int32 fd; uint32 type}
	pidFdSocket   = 3 // PROC_PIDFDSOCKETINFO
	fdTypeSocket  = 2 // PROX_FDTYPE_SOCKET

	sockFdInfoSize = 792 // sizeof(struct socket_fdinfo)
	familyAt       = 184 // psi.soi_family, an int32
	kindAt         = 256 // psi.soi_kind, an int32
	caddrAt        = 535 // psi.soi_proto.pri_un.unsi_caddr, a struct sockaddr_un
	afUnix         = 1   // AF_UNIX
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
// or that the caller may not read, has none.
func peers(pid int) []string {
	n, err := info(callPidInfo, pid, pidListFds, 0, nil)
	if err != nil || n <= 0 {
		return nil
	}
	b := make([]byte, n)
	for {
		m, err := info(callPidInfo, pid, pidListFds, 0, b)
		if err != nil {
			return nil
		}
		if m < len(b) {
			b = b[:m]
			break
		}
		b = make([]byte, 2*len(b)) // descriptors opened since the sizing call
	}
	var out []string
	s := make([]byte, sockFdInfoSize)
	for i := 0; i+8 <= len(b); i += 8 {
		fd, typ := int(int32(binary.LittleEndian.Uint32(b[i:]))), binary.LittleEndian.Uint32(b[i+4:])
		if typ != fdTypeSocket {
			continue
		}
		if m, err := info(callPidFdInfo, pid, pidFdSocket, fd, s); err != nil || m < len(s) {
			continue
		}
		if binary.LittleEndian.Uint32(s[familyAt:]) != afUnix || binary.LittleEndian.Uint32(s[kindAt:]) != sockinfoUn {
			continue
		}
		p := s[caddrAt+sunPathAt : caddrAt+sunPathAt+sunPathLen]
		if j := indexNUL(p); j >= 0 {
			p = p[:j]
		}
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}

// holdsDockerSocket reports whether pid holds a unix socket connected to one containerSocket accepts.
func holdsDockerSocket(pid int) bool { return slices.ContainsFunc(peers(pid), containerSocket) }

// indexNUL returns the index of the first NUL in b, or -1.
func indexNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}
