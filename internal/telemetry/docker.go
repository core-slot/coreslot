//go:build unix

package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/store"
)

// TCPHost stands for the one candidate socket when DOCKER_HOST is not a unix socket. It is also
// the code the line then carries: nothing is connected to.
const TCPHost = "tcp_host"

// The other closed codes of containers_err; a state error's text would embed a path.
const (
	errNoSocket = "no_socket"
	errConnect  = "connect"
	errDeadline = "deadline"
	errHTTP     = "http"
	errParse    = "parse"
)

const (
	containersBudget = 2 * time.Second // the whole read, from the connect to the last byte
	containersMax    = 16
	responseMax      = 1 << 20
	imageMax         = 64
)

// Container is one running container as `docker stats` shows it.
type Container struct {
	ID, Image          string
	UsageMiB, LimitMiB int64
}

// Connect opens the unix socket at path; a test replaces it.
var Connect = connect

// ContainersAsync writes a containers line when none was written in the last 30 s by any process,
// in a goroutine, so the caller never waits for the socket. The line names no run.
func (r *Recorder) ContainersAsync(sockets []string) {
	if r == nil || r.Stopped() || !r.claim(store.ContainersStamp) {
		return
	}
	r.memWG.Add(1)
	go func() {
		defer r.memWG.Done()
		defer r.recoverLost()
		list, total, code := Containers(time.Now().Add(containersBudget), sockets)
		items := make([]Fields, 0, len(list))
		for _, c := range list {
			items = append(items, Fields{"id": c.ID, "image": c.Image, "usage_mib": c.UsageMiB, "limit_mib": c.LimitMiB})
		}
		f := Fields{"containers": items, "containers_total": total}
		if code != "" {
			f["containers_err"] = code
		}
		r.EmitMachine("containers", f)
	}()
}

// Containers reads up to containersMax running containers from the first of sockets that answers,
// all of it against one deadline and with no retry. It returns what it read, how many are running
// and a closed code, "" for none. A container that cannot be read costs only itself; at the
// deadline the read ends with what it has.
func Containers(deadline time.Time, sockets []string) (list []Container, total int, errCode string) {
	if len(sockets) == 1 && sockets[0] == TCPHost {
		return nil, 0, TCPHost
	}
	var sock string
	var body []byte
	errCode = errNoSocket
	for _, s := range sockets {
		b, code := get(s, "/containers/json", deadline)
		if code == errNoSocket {
			continue
		}
		errCode = code
		if code != errConnect {
			sock, body = s, b
			break
		}
	}
	if sock == "" || errCode != "" {
		return nil, 0, errCode
	}
	var running []struct {
		ID    string `json:"Id"`
		Image string `json:"Image"`
	}
	if json.Unmarshal(body, &running) != nil {
		return nil, 0, errParse
	}
	total = len(running)
	for _, c := range running[:min(total, containersMax)] {
		b, code := get(sock, "/containers/"+c.ID+"/stats?stream=false&one-shot=true", deadline)
		var s dockerStats
		if code == "" && json.Unmarshal(b, &s) != nil {
			code = errParse
		}
		if code != "" {
			errCode = code
			if code == errDeadline {
				return list, total, errCode
			}
			continue
		}
		list = append(list, Container{ID: c.ID[:min(12, len(c.ID))], Image: c.Image[:min(imageMax, len(c.Image))],
			UsageMiB: int64(usedBytes(s) >> 20), LimitMiB: int64(s.MemoryStats.Limit >> 20)})
	}
	return list, total, errCode
}

type dockerStats struct {
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

// usedBytes is the usage `docker stats` shows: inactive file pages are left out, under the name of
// cgroup v1 and then of v2, only when they are smaller than the usage.
func usedBytes(s dockerStats) uint64 {
	m := s.MemoryStats
	if v, ok := m.Stats["total_inactive_file"]; ok && v < m.Usage {
		return m.Usage - v
	}
	if v := m.Stats["inactive_file"]; v < m.Usage {
		return m.Usage - v
	}
	return m.Usage
}

// get makes one HTTP/1.0 request on a connection of its own and returns the body of a 200.
func get(sock, target string, deadline time.Time) ([]byte, string) {
	fd, code := Connect(sock, deadline)
	if code != "" {
		return nil, code
	}
	defer unix.Close(fd)
	if code := send(fd, []byte("GET "+target+" HTTP/1.0\r\nHost: docker\r\n\r\n"), deadline); code != "" {
		return nil, code
	}
	resp, code := receive(fd, deadline)
	if code != "" {
		return nil, code
	}
	head, body, ok := bytes.Cut(resp, []byte("\r\n\r\n"))
	if status := bytes.Fields(head); !ok || len(status) < 2 || string(status[1]) != "200" {
		return nil, errHTTP
	}
	return body, ""
}

// connect opens a non-blocking stream socket to path. The socket is made and marked under the
// fork lock, since a child that inherited it would read as a container client.
func connect(path string, deadline time.Time) (int, string) {
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return -1, errConnect
	}
	if code := dial(fd, path, deadline); code != "" {
		unix.Close(fd)
		return -1, code
	}
	return fd, ""
}

func dial(fd int, path string, deadline time.Time) string {
	if unix.SetNonblock(fd, true) != nil {
		return errConnect
	}
	err := unix.Connect(fd, &unix.SockaddrUnix{Name: path})
	switch {
	case err == nil:
		return ""
	case errors.Is(err, unix.ENOENT):
		return errNoSocket
	case !errors.Is(err, unix.EINPROGRESS) && !errors.Is(err, unix.EINTR):
		return errConnect
	}
	if !ready(fd, unix.POLLOUT, deadline) {
		return errDeadline
	}
	if soErr, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR); err != nil || soErr != 0 {
		return errConnect
	}
	return ""
}

func send(fd int, b []byte, deadline time.Time) string {
	for len(b) > 0 {
		if !ready(fd, unix.POLLOUT, deadline) {
			return errDeadline
		}
		n, err := unix.Write(fd, b)
		if err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return errConnect
		}
		b = b[max(n, 0):]
	}
	return ""
}

// receive reads to EOF, at most responseMax bytes.
func receive(fd int, deadline time.Time) ([]byte, string) {
	var resp []byte
	buf := make([]byte, 32<<10)
	for {
		if !ready(fd, unix.POLLIN, deadline) {
			return nil, errDeadline
		}
		n, err := unix.Read(fd, buf)
		switch {
		case errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR):
		case err != nil:
			return nil, errConnect
		case n == 0:
			return resp, ""
		case len(resp)+n > responseMax:
			return nil, errHTTP
		default:
			resp = append(resp, buf[:n]...)
		}
	}
}

// ready waits until fd is ready for events; false means the deadline passed.
func ready(fd int, events int16, deadline time.Time) bool {
	pfd := []unix.PollFd{{Fd: int32(fd), Events: events}}
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		n, err := unix.Poll(pfd, int(left.Milliseconds())+1)
		if n > 0 {
			return true
		}
		if err != nil && !errors.Is(err, unix.EINTR) {
			return false
		}
	}
}
