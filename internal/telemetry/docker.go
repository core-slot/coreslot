//go:build unix

package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
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
	containersBudget = 2 * time.Second // the whole read, but for a connect resolving its path, which nothing interrupts
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

// ContainersAsync writes a containers line.
func (r *Recorder) ContainersAsync(sockets []string) {
	r.machineLine(store.ContainersStamp, "containers", func() Fields {
		start := time.Now()
		list, total, code := Containers(start.Add(containersBudget), sockets)
		readMS := time.Since(start).Milliseconds()
		items := make([]Fields, 0, len(list))
		for _, c := range list {
			items = append(items, Fields{"id": c.ID, "image": c.Image, "usage_mib": c.UsageMiB, "limit_mib": c.LimitMiB})
		}
		f := Fields{"containers": items, "containers_total": total, "read_ms": readMS}
		if code != "" {
			f["containers_err"] = code
		}
		return f
	})
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
		if s.MemoryStats.Limit == 0 { // it stopped after the list; its zeros are no reading
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
	fd, code := Connect(sock)
	if code != "" {
		return nil, code
	}
	f := os.NewFile(uintptr(fd), "docker socket") // non-blocking, so the runtime's poller takes it and a deadline holds
	defer f.Close()
	if f.SetDeadline(deadline) != nil {
		return nil, errConnect
	}
	var resp []byte
	_, err := f.WriteString("GET " + target + " HTTP/1.0\r\nHost: docker\r\n\r\n")
	if err == nil {
		resp, err = io.ReadAll(io.LimitReader(f, responseMax+1))
	}
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return nil, errDeadline
	case err != nil:
		return nil, errConnect
	case len(resp) > responseMax:
		return nil, errHTTP
	}
	head, body, ok := bytes.Cut(resp, []byte("\r\n\r\n"))
	if status := bytes.Fields(head); !ok || len(status) < 2 || string(status[1]) != "200" {
		return nil, errHTTP
	}
	return body, ""
}

// connect opens a non-blocking stream socket to path. The socket is made and marked under the
// fork lock, since a child that inherited it would read as a container client.
func connect(path string) (int, string) {
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return -1, errConnect
	}
	if code := dial(fd, path); code != "" {
		unix.Close(fd)
		return -1, code
	}
	return fd, ""
}

// dial connects fd without blocking: a unix socket's connect completes or fails at once.
func dial(fd int, path string) string {
	if unix.SetNonblock(fd, true) != nil {
		return errConnect
	}
	err := unix.Connect(fd, &unix.SockaddrUnix{Name: path})
	switch {
	case err == nil:
		return ""
	case errors.Is(err, unix.ENOENT):
		return errNoSocket
	}
	return errConnect
}
