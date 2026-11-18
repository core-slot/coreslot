//go:build unix

// Package dockertest is a fake Docker engine on a unix socket. It is for tests only and may use
// net, which no binary of coreslot may link: never import it from non-test code.
package dockertest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/docker"
)

// LongID is a container id as an engine makes it: 64 hexadecimal characters.
var LongID = strings.Repeat("0123456789abcdef", 4)

// ID is such an id of one digit repeated.
func ID(c byte) string { return strings.Repeat(string(c), 64) }

// Soon is a deadline 2 s from now.
func Soon() time.Time { return time.Now().Add(2 * time.Second) }

// Fake is a unix-socket server that answers each connection's one request with its handler.
type Fake struct {
	Sock  string // the socket's path
	conns atomic.Int64
	h     *httpEngine // the engine HTTP serves; nil for Serve's

	mu      sync.Mutex
	targets []string // the first line of every request, in order
}

// Requests are the first lines of the requests received, in order.
func (d *Fake) Requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.targets)
}

// Conns is the number of connections the listener accepted, whoever made them: a test counts the
// connections of a child process with it.
func (d *Fake) Conns() int64 { return d.conns.Load() }

// SocketDir is a directory short enough for sun_path, which t.TempDir exceeds.
func SocketDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// Serve answers each connection's one request with handle, which gets the request target.
func Serve(t testing.TB, handle func(c net.Conn, target string)) *Fake {
	t.Helper()
	d := &Fake{Sock: filepath.Join(SocketDir(t), "docker.sock")}
	ln, err := net.Listen("unix", d.Sock)
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
			d.conns.Add(1)
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

// jsonString is s as a JSON string; %q would write Go escapes (\x1b) that JSON does not have. The
// replacement character stands for an invalid byte, which goes out raw, as a careless engine's would.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return strings.ReplaceAll(string(b), "\ufffd", "\xff")
}

// Reply writes a response of the status with a JSON body.
func Reply(c net.Conn, status int, body string) {
	fmt.Fprintf(c, "HTTP/1.0 %d Any\r\nContent-Type: application/json\r\n\r\n%s", status, body)
}

// Stats is the body of a container's stats: usage and limit in bytes and the memory stats' extra keys.
func Stats(usage, limit int64, extra string) string {
	return fmt.Sprintf(`{"read":"x","memory_stats":{"usage":%d,"limit":%d,"stats":{%s}}}`, usage, limit, extra)
}

// Container is a running container of the fake engine, its memory in bytes as the engine reports it.
type Container struct {
	ID, Name, Image, Project                  string
	UsageBytes, InactiveFileBytes, LimitBytes uint64
}

type engine struct {
	os         string
	memTotal   int64
	containers []Container
	statsCode  map[string]int
	stopped    map[string]bool
	hang       bool
	delay      time.Duration
	pad        int
	done       chan struct{}
	cont       bool          // HTTP: a 100 Continue before every answer
	missing    bool          // HTTP: every create finds no image
	slowWait   time.Duration // HTTP: a wait answers after this
}

// Option shapes the engine Engine serves.
type Option func(*engine)

// WithInfo sets /info's OperatingSystem and MemTotal; the default is Docker Desktop with 8 GiB.
func WithInfo(operatingSystem string, memTotalBytes int64) Option {
	return func(e *engine) { e.os, e.memTotal = operatingSystem, memTotalBytes }
}

// WithContainer adds a running container; its stats carry memory_stats.usage, .stats.inactive_file
// and .limit.
func WithContainer(c Container) Option {
	return func(e *engine) { e.containers = append(e.containers, c) }
}

// StatsStatus makes the container's stats call answer code.
func StatsStatus(id string, code int) Option { return func(e *engine) { e.statsCode[id] = code } }

// StoppedAtStats lists the container and answers its stats 200 with a limit of 0, as an engine does
// for a container that stopped after the list.
func StoppedAtStats(id string) Option { return func(e *engine) { e.stopped[id] = true } }

// Hang makes every request wait for the test's end.
func Hang() Option { return func(e *engine) { e.hang = true } }

// Slow makes every response wait d: each call is within a deadline, all of them are not.
func Slow(d time.Duration) Option { return func(e *engine) { e.delay = d } }

// PadList makes the container list n bytes larger, in a field a reader ignores.
func PadList(n int) Option { return func(e *engine) { e.pad = n } }

// newEngine is the engine of the options, Docker Desktop with 8 GiB by default.
func newEngine(opts []Option) *engine {
	e := &engine{os: docker.DesktopOS, memTotal: 8 << 30, statsCode: map[string]int{}, stopped: map[string]bool{}, done: make(chan struct{})}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Engine serves an engine, which a test closes with its cleanup.
func Engine(t testing.TB, opts ...Option) *Fake {
	t.Helper()
	e := newEngine(opts)
	t.Cleanup(func() { close(e.done) })
	return Serve(t, func(c net.Conn, target string) {
		if e.hang {
			<-e.done
			return
		}
		time.Sleep(e.delay)
		switch {
		case target == "/info":
			Reply(c, 200, fmt.Sprintf(`{"OperatingSystem":%q,"MemTotal":%d}`, e.os, e.memTotal))
		case target == "/containers/json":
			pad := strings.Repeat("x", e.pad) // in the first item, a field a reader ignores
			var items []string
			for i, x := range e.containers {
				item := fmt.Sprintf(`{"Id":%s,"Names":[%s],"Image":%s,"Labels":{"com.docker.compose.project":%s},"State":"running"`,
					jsonString(x.ID), jsonString("/"+x.Name), jsonString(x.Image), jsonString(x.Project))
				if i == 0 && e.pad > 0 {
					item += `,"Pad":"` + pad + `"`
				}
				items = append(items, item+"}")
			}
			Reply(c, 200, "["+strings.Join(items, ",")+"]")
		default:
			for _, x := range e.containers {
				if path, _ := docker.StatsPath(x.ID); target != path {
					continue
				}
				switch {
				case e.statsCode[x.ID] != 0:
					Reply(c, e.statsCode[x.ID], `{"message":"status set by the test"}`)
				case e.stopped[x.ID]:
					Reply(c, 200, Stats(0, 0, ``))
				default:
					Reply(c, 200, Stats(int64(x.UsageBytes), int64(x.LimitBytes), fmt.Sprintf(`"inactive_file":%d`, x.InactiveFileBytes)))
				}
				return
			}
			Reply(c, 404, `{"message":"no such container"}`)
		}
	})
}
