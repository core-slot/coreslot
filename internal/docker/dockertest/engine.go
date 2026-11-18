//go:build unix

// Package dockertest is a fake Docker engine on a unix socket. It is for tests only and may use
// net, which no binary of coreslot may link: never import it from non-test code.
package dockertest

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/core-slot/coreslot/internal/docker"
)

// Fake is a unix-socket server that answers each connection's one request with its handler.
type Fake struct {
	Path  string
	Conns atomic.Int64

	mu      sync.Mutex
	targets []string // the first line of every request, in order
}

// Requests are the first lines of the requests received, in order.
func (d *Fake) Requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.targets)
}

// SocketDir is a directory short enough for sun_path, which t.TempDir exceeds.
func SocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// Serve answers each connection's one request with handle, which gets the request target.
func Serve(t *testing.T, handle func(c net.Conn, target string)) *Fake {
	t.Helper()
	d := &Fake{Path: filepath.Join(SocketDir(t), "docker.sock")}
	ln, err := net.Listen("unix", d.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			d.Conns.Add(1)
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				first, _ := br.ReadString('\n')
				if first == "" {
					return // connected and closed, with no request
				}
				for {
					l, err := br.ReadString('\n')
					if err != nil || l == "\r\n" {
						break
					}
				}
				d.mu.Lock()
				d.targets = append(d.targets, strings.TrimSpace(first))
				d.mu.Unlock()
				handle(c, strings.Fields(first)[1])
			}()
		}
	}()
	return d
}

// Reply writes a response of the status with a JSON body.
func Reply(c net.Conn, status int, body string) {
	fmt.Fprintf(c, "HTTP/1.0 %d Any\r\nContent-Type: application/json\r\n\r\n%s", status, body)
}

// Stats is the body of a container's stats: usage and limit in bytes and the memory stats' extra keys.
func Stats(usage, limit int64, extra string) string {
	return fmt.Sprintf(`{"read":"x","memory_stats":{"usage":%d,"limit":%d,"stats":{%s}}}`, usage, limit, extra)
}

type engine struct {
	os            string
	memTotal      int64
	containers    []docker.Container
	inactive      map[string]int64 // id to MiB of inactive file in the usage
	hang          bool
	pad           int
	stopped       string
	failStatsCall int64
	statsCalls    atomic.Int64
	done          chan struct{}
}

// Option shapes the engine Engine serves.
type Option func(*engine)

// WithInfo sets /info's OperatingSystem and MemTotal; the default is Docker Desktop with 8 GiB.
func WithInfo(os string, memTotalBytes int64) Option {
	return func(e *engine) { e.os, e.memTotal = os, memTotalBytes }
}

// WithContainer adds a running container; UsageMiB is its raw usage, before inactive file pages.
func WithContainer(c docker.Container) Option {
	return func(e *engine) { e.containers = append(e.containers, c) }
}

// WithInactiveFile puts mib of the container's usage in inactive file pages.
func WithInactiveFile(id string, mib int64) Option {
	return func(e *engine) { e.inactive[id] = mib }
}

// Hang makes every request wait for the test's end.
func Hang() Option { return func(e *engine) { e.hang = true } }

// HugeList makes the container list larger than any reading takes.
func HugeList() Option { return PadList(4 << 20) }

// PadList makes the container list n bytes larger, in a field a reader ignores.
func PadList(n int) Option { return func(e *engine) { e.pad = n } }

// StopBetweenListAndStats lists the container and answers its stats with zeros, as an engine does
// for a container that stopped after the list.
func StopBetweenListAndStats(id string) Option { return func(e *engine) { e.stopped = id } }

// FailStatsOnCall answers the nth stats request (from 1) with a 500.
func FailStatsOnCall(n int64) Option { return func(e *engine) { e.failStatsCall = n } }

// Engine serves an engine and returns its socket.
func Engine(t *testing.T, opts ...Option) (sock string) { return EngineFake(t, opts...).Path }

// EngineFake is Engine, returning the server for its request log.
func EngineFake(t *testing.T, opts ...Option) *Fake {
	t.Helper()
	e := &engine{os: docker.DesktopOS, memTotal: 8 << 30, inactive: map[string]int64{}, done: make(chan struct{})}
	for _, o := range opts {
		o(e)
	}
	t.Cleanup(func() { close(e.done) })
	return Serve(t, func(c net.Conn, target string) {
		if e.hang {
			<-e.done
			return
		}
		switch {
		case target == "/info":
			Reply(c, 200, fmt.Sprintf(`{"OperatingSystem":%q,"MemTotal":%d}`, e.os, e.memTotal))
		case target == "/containers/json":
			pad := strings.Repeat("x", e.pad) // in the first item, a field a reader ignores
			var items []string
			for i, x := range e.containers {
				item := fmt.Sprintf(`{"Id":%q,"Names":["/%s"],"Image":%q,"Labels":{"com.docker.compose.project":%q},"State":"running"`,
					x.ID, x.Name, x.Image, x.Project)
				if i == 0 && e.pad > 0 {
					item += `,"Pad":"` + pad + `"`
				}
				items = append(items, item+"}")
			}
			if len(items) == 0 && e.pad > 0 {
				items = append(items, `{"Id":"pad","Names":[],"Image":"","Pad":"`+pad+`"}`)
			}
			Reply(c, 200, "["+strings.Join(items, ",")+"]")
		default:
			for _, x := range e.containers {
				if target != docker.StatsPath(x.ID) {
					continue
				}
				if n := e.statsCalls.Add(1); n == e.failStatsCall {
					Reply(c, 500, `{"message":"boom"}`)
				} else if x.ID == e.stopped {
					Reply(c, 200, Stats(0, 0, ``))
				} else {
					Reply(c, 200, Stats(x.UsageMiB<<20, x.LimitMiB<<20, fmt.Sprintf(`"inactive_file":%d`, e.inactive[x.ID]<<20)))
				}
				return
			}
			Reply(c, 404, `{"message":"no such container"}`)
		}
	})
}
