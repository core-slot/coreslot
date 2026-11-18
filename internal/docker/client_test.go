//go:build unix

package docker_test

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
)

// The socket does not reach a child: a command that inherited it would read as a container client.
// It is non-blocking, or no deadline bounds it. Mutations: no CloseOnExec; no SetNonblock.
func TestSocketIsCloseOnExec(t *testing.T) {
	f := dockertest.Engine(t)
	fd, code := docker.Connect(f.Sock)
	if code != "" {
		t.Fatalf("code %q", code)
	}
	defer unix.Close(fd)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Errorf("F_GETFD %#x, %v: not close-on-exec", flags, err)
	}
	fl, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil || fl&unix.O_NONBLOCK == 0 {
		t.Errorf("F_GETFL %#x, %v: blocking", fl, err)
	}
}

// A response is read up to max bytes, and a longer one is refused rather than held. Mutations: no
// cap returns the second row's body with no code; a cap of 1 KiB reddens the first.
func TestResponseCap(t *testing.T) {
	head := "HTTP/1.0 200 OK\r\n\r\n"
	for _, c := range []struct {
		name string
		size int
		want string
	}{{"just under", 1<<20 - len(head), ""}, {"over", 1<<20 + 4096, "http"}} {
		t.Run(c.name, func(t *testing.T) {
			f := dockertest.Serve(t, func(conn net.Conn, _ string) {
				conn.Write([]byte(head + "[" + strings.Repeat(" ", c.size-2) + "]"))
			})
			if _, code := docker.Get(f.Sock, "/x", soon(), 1<<20); code != c.want {
				t.Errorf("code %q, want %q", code, c.want)
			}
		})
	}
}

// Junk is a closed code, and a status other than 200 is its own. Mutations: ignoring the status
// parses the 500's body; a stats body of the wrong type read as zeros lists the container.
func TestJunkBodyIsParseCode(t *testing.T) {
	for _, c := range []struct {
		name, list string
		code       int
		stats      string
		want       string
	}{
		{"list is junk", "not json", 200, "", "parse"},
		{"list is the wrong shape", `{"Id":"x"}`, 200, "", "parse"},
		{"list refused", `[]`, 500, "", "http"},
		{"no status line", "", 0, "", "http"},
		{"stats is junk", "", 200, "{", "parse"},
		{"stats is the wrong type", "", 200, `{"memory_stats":{"usage":"lots"}}`, "parse"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := dockertest.Serve(t, func(conn net.Conn, target string) {
				switch {
				case target == "/info":
					dockertest.Reply(conn, 200, `{"OperatingSystem":"Docker Desktop","MemTotal":1073741824}`)
				case c.code == 0:
					conn.Write([]byte("garbage"))
				case target == "/containers/json" && c.list != "":
					dockertest.Reply(conn, c.code, c.list)
				case target == "/containers/json":
					dockertest.Reply(conn, 200, `[{"Id":"`+cid('b')+`","Image":"b"}]`)
				default:
					dockertest.Reply(conn, 200, c.stats)
				}
			})
			r := docker.Read([]string{f.Sock}, soon())
			if r.OK || r.Code != c.want || r.Containers != nil {
				t.Errorf("reading %+v, want none with %q", r, c.want)
			}
		})
	}
}

// No socket at any candidate is its own code, not a connect failure. Mutation: treating ENOENT
// like every other error says connect.
func TestNoSocketIsACode(t *testing.T) {
	dir := dockertest.SocketDir(t)
	if r := docker.Read([]string{dir + "/a.sock", dir + "/b.sock"}, soon()); r.OK || r.Code != "no_socket" {
		t.Errorf("reading %+v, want no_socket", r)
	}
	if r := docker.Read(nil, soon()); r.Code != "no_socket" {
		t.Errorf("no candidates: %+v", r)
	}
}

// A refused connection is a closed code with no path, and the next candidate is still tried; the
// first that answers is used. Mutations: stopping at the first error reddens the second part;
// returning the error's text instead of the code puts the path in the code.
func TestConnectErrorHasNoPath(t *testing.T) {
	dir := dockertest.SocketDir(t)
	refused := filepath.Join(dir, "refused.sock")
	ln, err := net.Listen("unix", refused)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	r := docker.Read([]string{refused, dir + "/absent.sock"}, soon())
	if r.OK || r.Code != "connect" || strings.Contains(r.Code, dir) {
		t.Errorf("reading %+v, want connect", r)
	}
	f := dockertest.Engine(t)
	if r := docker.Read([]string{refused, f.Sock}, soon()); !r.OK {
		t.Errorf("a live candidate after a refused one: %+v", r)
	}
}

// Usage is what `docker stats` shows. Mutations: subtracting without the smaller-than test wraps
// around in the third and fifth rows; reading only inactive_file reddens v1; falling out of the v1
// branch without trying v2 reddens the fourth.
func TestUsageMirrorsDockerStats(t *testing.T) {
	const m = 1 << 20
	for _, c := range []struct {
		name  string
		extra string
		want  uint64
	}{
		{"cgroup v1", fmt.Sprintf(`"total_inactive_file":%d,"inactive_file":%d`, 4*m, 9*m), 6 * m},
		{"cgroup v2", fmt.Sprintf(`"inactive_file":%d`, 3*m), 7 * m},
		{"inactive above usage", fmt.Sprintf(`"inactive_file":%d`, 11*m), 10 * m},
		{"v1 above usage falls through to v2", fmt.Sprintf(`"total_inactive_file":%d,"inactive_file":%d`, 20*m, 3*m), 7 * m},
		{"inactive equal to usage", fmt.Sprintf(`"inactive_file":%d`, 10*m), 10 * m},
	} {
		t.Run(c.name, func(t *testing.T) {
			var s docker.Stats
			if err := json.Unmarshal([]byte(dockertest.Stats(10*m, 100*m, c.extra)), &s); err != nil {
				t.Fatal(err)
			}
			if got := docker.UsedBytes(s); got != c.want {
				t.Errorf("usage %d, want %d", got, c.want)
			}
		})
	}
}
