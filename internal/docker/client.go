//go:build unix

// Package docker reads a local Docker engine over its unix socket: GET requests only, no
// subprocess, every read against a deadline of the caller's.
package docker

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The other closed codes of a failed read; a state error's text would embed a path.
const (
	NoSocket   = "no_socket"
	ConnectErr = "connect"
	Deadline   = "deadline"
	httpErr    = "http"
	Parse      = "parse"
)

// Connect opens the unix socket at path; a test replaces it.
var Connect = connect

// Get makes one HTTP/1.0 GET on a connection of its own and returns the body of a 200. A response
// larger than max bytes is no response: code "http".
func Get(sock, target string, deadline time.Time, max int64) ([]byte, string) {
	body, _, code := do(sock, target, deadline, max)
	return body, code
}

// do is Get, with the status line's code (0 when there is none) for the caller that treats one
// status apart; a status other than 200 comes with code HTTP.
func do(sock, target string, deadline time.Time, max int64) (body []byte, status int, code string) {
	fd, code := Connect(sock)
	if code != "" {
		return nil, 0, code
	}
	f := os.NewFile(uintptr(fd), "docker socket") // non-blocking, so the runtime's poller takes it and a deadline holds
	defer f.Close()
	if f.SetDeadline(deadline) != nil {
		return nil, 0, ConnectErr
	}
	var resp []byte
	_, err := f.WriteString("GET " + target + " HTTP/1.0\r\nHost: docker\r\n\r\n")
	if err == nil {
		resp, err = io.ReadAll(io.LimitReader(f, max+1))
	}
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return nil, 0, Deadline
	case err != nil:
		return nil, 0, ConnectErr
	case int64(len(resp)) > max:
		return nil, 0, httpErr
	}
	head, body, ok := bytes.Cut(resp, []byte("\r\n\r\n"))
	fields := bytes.Fields(head)
	if !ok || len(fields) < 2 {
		return nil, 0, httpErr
	}
	status, _ = strconv.Atoi(string(fields[1]))
	if status != 200 {
		return nil, status, httpErr
	}
	return body, status, ""
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
		return -1, ConnectErr
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
		return ConnectErr
	}
	err := unix.Connect(fd, &unix.SockaddrUnix{Name: path})
	switch {
	case err == nil:
		return ""
	case errors.Is(err, unix.ENOENT):
		return NoSocket
	}
	return ConnectErr
}
