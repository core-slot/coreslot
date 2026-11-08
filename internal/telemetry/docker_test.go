//go:build unix

package telemetry

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeDocker is a unix-socket server that answers each connection's one request with handle.
type fakeDocker struct {
	path  string
	conns atomic.Int64

	mu      sync.Mutex
	targets []string // the first line of every request, in order
}

func (d *fakeDocker) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.targets)
}

// socketDir is a directory short enough for sun_path, which t.TempDir exceeds.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func serveDocker(t *testing.T, handle func(c net.Conn, target string)) *fakeDocker {
	t.Helper()
	d := &fakeDocker{path: filepath.Join(socketDir(t), "docker.sock")}
	ln, err := net.Listen("unix", d.path)
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

func reply(c net.Conn, status int, body string) {
	fmt.Fprintf(c, "HTTP/1.0 %d Any\r\nContent-Type: application/json\r\n\r\n%s", status, body)
}

type fakeContainer struct{ id, image, stats string }

func stats(usage, limit int64, extra string) string {
	return fmt.Sprintf(`{"read":"x","memory_stats":{"usage":%d,"limit":%d,"stats":{%s}}}`, usage, limit, extra)
}

func statsPath(id string) string { return "/containers/" + id + "/stats?stream=false&one-shot=true" }

// engine answers as a Docker daemon with these containers running.
func engine(t *testing.T, cs ...fakeContainer) *fakeDocker {
	t.Helper()
	return serveDocker(t, func(c net.Conn, target string) {
		if target == "/containers/json" {
			var items []string
			for _, x := range cs {
				items = append(items, fmt.Sprintf(`{"Id":%q,"Image":%q,"State":"running"}`, x.id, x.image))
			}
			reply(c, 200, "["+strings.Join(items, ",")+"]")
			return
		}
		for _, x := range cs {
			if target == statsPath(x.id) {
				reply(c, 200, x.stats)
				return
			}
		}
		reply(c, 404, `{"message":"no such container"}`)
	})
}

func soon() time.Time { return time.Now().Add(2 * time.Second) }

const longID = "0123456789abcdef0123456789abcdef"

// The list, then one stats request per container and nothing else, each over HTTP/1.0 with the
// id cut to 12 and a shorter id whole. Mutations: an id cut without looking at its length panics;
// a stats path without one-shot=true reddens the requests; limit taken from usage reddens the list.
func TestContainersParsesStats(t *testing.T) {
	d := engine(t,
		fakeContainer{longID, "postgres:16", stats(300<<20, 2048<<20, `"inactive_file":100`)},
		fakeContainer{"abc", "alpine", stats(5<<20, 1024<<20, ``)})
	list, total, code := Containers(soon(), []string{d.path})
	want := []Container{{ID: longID[:12], Image: "postgres:16", UsageMiB: 299, LimitMiB: 2048}, {ID: "abc", Image: "alpine", UsageMiB: 5, LimitMiB: 1024}}
	if !reflect.DeepEqual(list, want) || total != 2 || code != "" {
		t.Errorf("list %+v total %d code %q, want %+v 2 and none", list, total, code, want)
	}
	reqs := d.requests()
	wantReqs := []string{"GET /containers/json HTTP/1.0", "GET " + statsPath(longID) + " HTTP/1.0", "GET " + statsPath("abc") + " HTTP/1.0"}
	if !reflect.DeepEqual(reqs, wantReqs) {
		t.Errorf("requests %q, want %q", reqs, wantReqs)
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
		want  int64
	}{
		{"cgroup v1", fmt.Sprintf(`"total_inactive_file":%d,"inactive_file":%d`, 4*m, 9*m), 6},
		{"cgroup v2", fmt.Sprintf(`"inactive_file":%d`, 3*m), 7},
		{"inactive above usage", fmt.Sprintf(`"inactive_file":%d`, 11*m), 10},
		{"v1 above usage falls through to v2", fmt.Sprintf(`"total_inactive_file":%d,"inactive_file":%d`, 20*m, 3*m), 7},
		{"inactive equal to usage", fmt.Sprintf(`"inactive_file":%d`, 10*m), 10},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := engine(t, fakeContainer{"abc", "alpine", stats(10*m, 100*m, c.extra)})
			list, _, code := Containers(soon(), []string{d.path})
			if code != "" || len(list) != 1 || list[0].UsageMiB != c.want {
				t.Errorf("list %+v code %q, want usage %d", list, code, c.want)
			}
		})
	}
}

// Sixteen are read and all are counted, and the longest line of sixteen fits. Mutations: no cap
// makes 21 requests; total taken after the cap says 16; an image left at full length makes the
// line too long, so nothing is written.
func TestContainersCappedAtSixteen(t *testing.T) {
	var cs []fakeContainer
	image := strings.Repeat("i", 200)
	for i := range 20 {
		cs = append(cs, fakeContainer{fmt.Sprintf("%064d", i), image, stats(1<<40, 1<<40, ``)})
	}
	d := engine(t, cs...)
	list, total, code := Containers(soon(), []string{d.path})
	if len(list) != 16 || total != 20 || code != "" {
		t.Fatalf("%d read of %d, code %q; want 16 of 20", len(list), total, code)
	}
	if n := len(d.requests()); n != 17 {
		t.Errorf("%d requests, want 17", n)
	}
	if len(list[0].Image) != 64 {
		t.Errorf("image of %d bytes, want 64", len(list[0].Image))
	}
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	r.ContainersAsync([]string{d.path})
	r.memWG.Wait()
	got := lines(t, dir)
	if len(got) != 1 || got[0]["containers_total"] != float64(20) || len(got[0]["containers"].([]any)) != 16 {
		t.Errorf("line %v", got)
	}
}

// A server that accepts and then says nothing costs the deadline and no more, and what was read
// before it is kept. Mutations: a poll with no timeout hangs the test; returning nil at the deadline
// loses the first row of the second case.
func TestContainersHungSocketReturnsWithinDeadline(t *testing.T) {
	for _, c := range []struct {
		name string
		read int // stats requests answered before the server hangs
	}{{"at the list", -1}, {"at the second container", 1}} {
		t.Run(c.name, func(t *testing.T) {
			done := make(chan struct{})
			defer close(done)
			var answered atomic.Int64
			d := serveDocker(t, func(conn net.Conn, target string) {
				if target == "/containers/json" && c.read >= 0 {
					reply(conn, 200, `[{"Id":"aaa","Image":"a"},{"Id":"bbb","Image":"b"}]`)
					return
				}
				if c.read >= 0 && answered.Add(1) <= int64(c.read) {
					reply(conn, 200, stats(1<<20, 1<<20, ``))
					return
				}
				<-done
			})
			start := time.Now()
			list, total, code := Containers(start.Add(2*time.Second), []string{d.path})
			if took := time.Since(start); took > 2500*time.Millisecond {
				t.Errorf("took %v", took)
			}
			if code != "deadline" || len(list) != max(c.read, 0) || (c.read >= 0 && total != 2) {
				t.Errorf("list %+v total %d code %q", list, total, code)
			}
		})
	}
}

// A server that answers one byte at a time cannot extend the deadline: it is one deadline for the
// whole read. Mutation: a deadline taken afresh per read never returns.
func TestContainersTricklingServerMissesNoDeadline(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	d := serveDocker(t, func(c net.Conn, _ string) {
		for {
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
				if _, err := c.Write([]byte("H")); err != nil {
					return
				}
			}
		}
	})
	start := time.Now()
	_, _, code := Containers(start.Add(time.Second), []string{d.path})
	if took := time.Since(start); took > 1500*time.Millisecond || code != "deadline" {
		t.Errorf("took %v, code %q", took, code)
	}
}

// A response is read up to 1 MiB, and a longer one is refused rather than held. Mutations: no cap
// parses the second row's array and gives no code; a cap of 1 KiB reddens the first.
func TestResponseCap(t *testing.T) {
	head := "HTTP/1.0 200 OK\r\n\r\n"
	for _, c := range []struct {
		name string
		size int
		want string
	}{{"just under", 1<<20 - len(head), ""}, {"over", 1<<20 + 4096, "http"}} {
		t.Run(c.name, func(t *testing.T) {
			d := serveDocker(t, func(conn net.Conn, _ string) {
				conn.Write([]byte(head + "[" + strings.Repeat(" ", c.size-2) + "]"))
			})
			if _, _, code := Containers(soon(), []string{d.path}); code != c.want {
				t.Errorf("code %q, want %q", code, c.want)
			}
		})
	}
}

// Junk is a closed code. A container that fails costs itself, not the ones after it; a status
// other than 200 is its own code. Mutations: a failed stats ending the loop loses the row's good
// container; ignoring the status parses the 500's body.
func TestJunkBodyIsParseCode(t *testing.T) {
	good := fakeContainer{"ggg", "good", stats(2<<20, 4<<20, ``)}
	for _, c := range []struct {
		name  string
		list  string
		code  int
		stats string
		want  string
		kept  int
	}{
		{"list is junk", "not json", 200, "", "parse", 0},
		{"list is the wrong shape", `{"Id":"x"}`, 200, "", "parse", 0},
		{"list refused", `[]`, 500, "", "http", 0},
		{"no status line", "", 0, "", "http", 0},
		{"stats is junk", "", 200, "{", "parse", 1},
		{"stats is the wrong type", "", 200, `{"memory_stats":{"usage":"lots"}}`, "parse", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := serveDocker(t, func(conn net.Conn, target string) {
				switch {
				case c.code == 0:
					conn.Write([]byte("garbage"))
				case target == "/containers/json" && c.list != "":
					reply(conn, c.code, c.list)
				case target == "/containers/json":
					reply(conn, 200, `[{"Id":"bad","Image":"b"},{"Id":"ggg","Image":"good"}]`)
				case target == statsPath("bad"):
					reply(conn, 200, c.stats)
				default:
					reply(conn, 200, good.stats)
				}
			})
			list, _, code := Containers(soon(), []string{d.path})
			if code != c.want || len(list) != c.kept {
				t.Errorf("list %+v code %q, want %d and %q", list, code, c.kept, c.want)
			}
		})
	}
}

// No socket at any candidate is its own code, not a connect failure. Mutation: treating ENOENT
// like every other error says connect.
func TestNoSocketIsACode(t *testing.T) {
	dir := socketDir(t)
	if _, total, code := Containers(soon(), []string{dir + "/a.sock", dir + "/b.sock"}); code != "no_socket" || total != 0 {
		t.Errorf("code %q total %d", code, total)
	}
	if _, _, code := Containers(soon(), nil); code != "no_socket" {
		t.Errorf("no candidates: code %q", code)
	}
}

// A refused connection is a closed code and the line holds no path; the next candidate is still
// tried, and the first that answers is used for the rest. Mutations: stopping at the first error
// reddens the second part; writing the error's text instead of the code puts the path on the line.
func TestConnectErrorHasNoPath(t *testing.T) {
	dir := socketDir(t)
	refused := filepath.Join(dir, "refused.sock")
	ln, err := net.Listen("unix", refused)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	state := stateDir(t)
	r := New(state, "run-1", "x")
	r.ContainersAsync([]string{refused, dir + "/absent.sock"})
	r.memWG.Wait()
	got := lines(t, state)
	if len(got) != 1 || got[0]["containers_err"] != "connect" {
		t.Fatalf("lines %v, want containers_err connect", got)
	}
	data, err := os.ReadFile(eventsPath(state))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), dir) || strings.Contains(string(data), "sock") {
		t.Errorf("the line carries the path: %s", data)
	}
	d := engine(t, fakeContainer{"abc", "alpine", stats(1<<20, 2<<20, ``)})
	if list, _, code := Containers(soon(), []string{refused, d.path}); code != "" || len(list) != 1 {
		t.Errorf("next candidate: list %+v code %q", list, code)
	}
	if n := len(d.requests()); n != 2 {
		t.Errorf("%d requests to the second candidate, want 2", n)
	}
}

// replaceConnect makes the recorder connect with fn; the test's end restores it.
func replaceConnect(t *testing.T, fn func(string, time.Time) (int, string)) {
	t.Helper()
	saved := Connect
	Connect = fn
	t.Cleanup(func() { Connect = saved })
}

// A DOCKER_HOST over TCP is a code, with no connection of any kind. Mutation: leaving the test out
// connects to the marker as a path.
func TestTCPDockerHost(t *testing.T) {
	var calls atomic.Int64
	replaceConnect(t, func(string, time.Time) (int, string) { calls.Add(1); return -1, "connect" })
	if list, total, code := Containers(soon(), []string{TCPHost}); code != "tcp_host" || total != 0 || list != nil {
		t.Errorf("list %v total %d code %q", list, total, code)
	}
	if calls.Load() != 0 {
		t.Errorf("%d connections", calls.Load())
	}
}

// Review Focus: a hung socket still gives a line within the deadline, and the caller does not wait
// for it. Mutations: reading in the caller takes two seconds; a line written only on success loses it.
func TestHungSocketLineStillWritten(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	d := serveDocker(t, func(net.Conn, string) { <-done })
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	start := time.Now()
	r.ContainersAsync([]string{d.path})
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("ContainersAsync took %v", took)
	}
	r.memWG.Wait()
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Errorf("the line took %v", took)
	}
	got := lines(t, dir)
	if len(got) != 1 || got[0]["e"] != "containers" || got[0]["containers_err"] != "deadline" {
		t.Fatalf("lines %v", got)
	}
	if l, ok := got[0]["containers"].([]any); !ok || len(l) != 0 || got[0]["containers_total"] != float64(0) {
		t.Errorf("line %v, want an empty list and a total of 0", got[0])
	}
}

// The line names no run and carries its own stamp, which the memory line's does not touch; one
// due claim, one read. Mutations: claiming the memory stamp leaves no containers line; not claiming
// reads twice; writing with Emit gives the line a run.
func TestContainersLineOncePerWindow(t *testing.T) {
	fakeSweep(t, goodSweep)
	d := engine(t, fakeContainer{longID, "postgres:16", stats(300<<20, 2048<<20, ``)})
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	r.MemoryAsync()
	r.memWG.Wait()
	r.ContainersAsync([]string{d.path})
	r.memWG.Wait()
	r.ContainersAsync([]string{d.path})
	r.memWG.Wait()
	got := lines(t, dir)
	if len(got) != 2 || d.conns.Load() != 2 {
		t.Fatalf("%d lines and %d connections, want 2 and 2: %v", len(got), d.conns.Load(), got)
	}
	l := got[1]
	if l["e"] != "containers" || l["run"] != nil || l["n"] != nil || l["containers_total"] != float64(1) {
		t.Errorf("line %v", l)
	}
	if _, has := l["containers_err"]; has {
		t.Errorf("a clean read has containers_err: %v", l)
	}
	c := l["containers"].([]any)[0].(map[string]any)
	if c["id"] != longID[:12] || c["image"] != "postgres:16" || c["usage_mib"] != float64(300) || c["limit_mib"] != float64(2048) {
		t.Errorf("container %v", c)
	}
	var nilRec *Recorder
	nilRec.ContainersAsync([]string{d.path})
	stopped := New(stateDir(t), "run-2", "x")
	stopped.Stop()
	stopped.ContainersAsync([]string{d.path})
	stopped.memWG.Wait()
	if n := d.conns.Load(); n != 2 {
		t.Errorf("%d connections, want none from a nil or stopped recorder", n-2)
	}
}

// A panic in the read is recovered and counted in the next line's lost. Mutation: dropping the
// recover in the goroutine kills the test binary.
func TestContainersPanicRecovered(t *testing.T) {
	replaceConnect(t, func(string, time.Time) (int, string) { panic("socket") })
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	r.ContainersAsync([]string{"/tmp/docker.sock"})
	r.memWG.Wait()
	r.Emit("admitted", nil)
	if got := lines(t, dir); len(got) != 1 || got[0]["lost"] != float64(1) {
		t.Errorf("lines %v; want admitted with lost 1", got)
	}
}

// The socket does not reach a child: a command that inherited it would read as a container client.
// It is non-blocking, or a poll is not what bounds it. Mutations: no CloseOnExec; no SetNonblock.
func TestSocketIsCloseOnExec(t *testing.T) {
	d := engine(t)
	fd, code := connect(d.path, soon())
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

// The binary reaches the socket by raw syscalls alone. Mutation: importing net in docker.go.
func TestNoNetInBinary(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./cmd/coreslot")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, out)
	}
	deps := strings.Fields(string(out))
	if !slices.Contains(deps, "golang.org/x/sys/unix") {
		t.Error("golang.org/x/sys/unix is not among the dependencies")
	}
	for _, p := range deps {
		if p == "net" || p == "net/http" || p == "crypto" || strings.HasPrefix(p, "crypto/") {
			t.Errorf("the binary depends on %s", p)
		}
	}
}
