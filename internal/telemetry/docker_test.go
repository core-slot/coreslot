//go:build unix

package telemetry

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
)

type fakeContainer struct{ id, image, stats string }

// engine answers as a Docker daemon with these containers running.
func engine(t *testing.T, cs ...fakeContainer) *dockertest.Fake {
	t.Helper()
	return dockertest.Serve(t, func(c net.Conn, target string) {
		if target == "/containers/json" {
			var items []string
			for _, x := range cs {
				items = append(items, fmt.Sprintf(`{"Id":%q,"Image":%q,"State":"running"}`, x.id, x.image))
			}
			dockertest.Reply(c, 200, "["+strings.Join(items, ",")+"]")
			return
		}
		for _, x := range cs {
			if target == docker.StatsPath(x.id) {
				dockertest.Reply(c, 200, x.stats)
				return
			}
		}
		dockertest.Reply(c, 404, `{"message":"no such container"}`)
	})
}

func soon() time.Time { return time.Now().Add(2 * time.Second) }

const longID = "0123456789abcdef0123456789abcdef"

// The list, then one stats request per container and nothing else, each over HTTP/1.0 with the
// id cut to 12 and a shorter id whole. Mutations: an id cut without looking at its length panics;
// a stats path without one-shot=true reddens the requests; limit taken from usage reddens the list.
func TestContainersParsesStats(t *testing.T) {
	d := engine(t,
		fakeContainer{longID, "postgres:16", dockertest.Stats(300<<20, 2048<<20, `"inactive_file":100`)},
		fakeContainer{"abc", "alpine", dockertest.Stats(5<<20, 1024<<20, ``)})
	list, total, code := Containers(soon(), []string{d.Sock})
	want := []docker.Container{{ID: longID[:12], Image: "postgres:16", UsageMiB: 299, LimitMiB: 2048}, {ID: "abc", Image: "alpine", UsageMiB: 5, LimitMiB: 1024}}
	if !reflect.DeepEqual(list, want) || total != 2 || code != "" {
		t.Errorf("list %+v total %d code %q, want %+v 2 and none", list, total, code, want)
	}
	reqs := d.Requests()
	wantReqs := []string{"GET /containers/json HTTP/1.0", "GET " + docker.StatsPath(longID) + " HTTP/1.0", "GET " + docker.StatsPath("abc") + " HTTP/1.0"}
	if !reflect.DeepEqual(reqs, wantReqs) {
		t.Errorf("requests %q, want %q", reqs, wantReqs)
	}
}

// A container that stopped between the list and its stats answers with zeros: it is counted in the
// total, left out of the list and is no error. Mutation: without the limit test it is listed at 0.
func TestStoppedContainerLeftOut(t *testing.T) {
	d := engine(t, fakeContainer{"gone", "alpine", dockertest.Stats(0, 0, ``)}, fakeContainer{"abc", "alpine", dockertest.Stats(5<<20, 1024<<20, ``)})
	list, total, code := Containers(time.Now().Add(2*time.Second), []string{d.Sock})
	if code != "" || total != 2 || len(list) != 1 || list[0].ID != "abc" {
		t.Errorf("list %v, total %d, code %q; want only abc of 2, no code", list, total, code)
	}
}

// Sixteen are read and all are counted, and the longest line of sixteen fits. Mutations: no cap
// makes 21 requests; total taken after the cap says 16; an image left at full length makes the
// line too long, so nothing is written.
func TestContainersCappedAtSixteen(t *testing.T) {
	var cs []fakeContainer
	image := strings.Repeat("i", 200)
	for i := range 20 {
		cs = append(cs, fakeContainer{fmt.Sprintf("%064d", i), image, dockertest.Stats(1<<40, 1<<40, ``)})
	}
	d := engine(t, cs...)
	list, total, code := Containers(soon(), []string{d.Sock})
	if len(list) != 16 || total != 20 || code != "" {
		t.Fatalf("%d read of %d, code %q; want 16 of 20", len(list), total, code)
	}
	if n := len(d.Requests()); n != 17 {
		t.Errorf("%d requests, want 17", n)
	}
	if len(list[0].Image) != 64 {
		t.Errorf("image of %d bytes, want 64", len(list[0].Image))
	}
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	r.ContainersAsync([]string{d.Sock})
	r.memWG.Wait()
	got := lines(t, dir)
	if len(got) != 1 || got[0]["containers_total"] != float64(20) || len(got[0]["containers"].([]any)) != 16 {
		t.Errorf("line %v", got)
	}
}

// A server that accepts and then says nothing costs the deadline and no more, and what was read
// before it is kept. Mutations: no deadline on the socket hangs the test; returning nil at the
// deadline loses what was read.
func TestContainersHungSocketReturnsWithinDeadline(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	var answered atomic.Int64
	d := dockertest.Serve(t, func(conn net.Conn, target string) {
		if target == "/containers/json" {
			dockertest.Reply(conn, 200, `[{"Id":"aaa","Image":"a"},{"Id":"bbb","Image":"b"}]`)
			return
		}
		if answered.Add(1) <= 1 {
			dockertest.Reply(conn, 200, dockertest.Stats(1<<20, 1<<20, ``))
			return
		}
		<-done
	})
	start := time.Now()
	list, total, code := Containers(start.Add(2*time.Second), []string{d.Sock})
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Errorf("took %v", took)
	}
	if code != "deadline" || len(list) != 1 || total != 2 {
		t.Errorf("list %+v total %d code %q", list, total, code)
	}
}

// A deadline already past before a request is written is the deadline, not a connection that failed:
// the read ends there rather than trying every other container. Mutation: a failed write read as connect.
func TestContainersPastDeadline(t *testing.T) {
	d := dockertest.Serve(t, func(conn net.Conn, _ string) { dockertest.Reply(conn, 200, `[]`) })
	if _, _, code := Containers(time.Now().Add(-time.Second), []string{d.Sock}); code != "deadline" {
		t.Errorf("code %q, want deadline", code)
	}
}

// A server that answers one byte at a time cannot extend the deadline: it is one deadline for the
// whole read. Mutation: a deadline taken afresh per read never returns.
func TestContainersTricklingServerMissesNoDeadline(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	d := dockertest.Serve(t, func(c net.Conn, _ string) {
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
	_, _, code := Containers(start.Add(time.Second), []string{d.Sock})
	if took := time.Since(start); took > 1500*time.Millisecond || code != "deadline" {
		t.Errorf("took %v, code %q", took, code)
	}
}

// Junk is a closed code. A container that fails costs itself, not the ones after it; a status
// other than 200 is its own code. Mutations: a failed stats ending the loop loses the row's good
// container; ignoring the status parses the 500's body.
func TestJunkBodyIsParseCode(t *testing.T) {
	good := fakeContainer{"ggg", "good", dockertest.Stats(2<<20, 4<<20, ``)}
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
			d := dockertest.Serve(t, func(conn net.Conn, target string) {
				switch {
				case c.code == 0:
					conn.Write([]byte("garbage"))
				case target == "/containers/json" && c.list != "":
					dockertest.Reply(conn, c.code, c.list)
				case target == "/containers/json":
					dockertest.Reply(conn, 200, `[{"Id":"bad","Image":"b"},{"Id":"ggg","Image":"good"}]`)
				case target == docker.StatsPath("bad"):
					dockertest.Reply(conn, 200, c.stats)
				default:
					dockertest.Reply(conn, 200, good.stats)
				}
			})
			list, _, code := Containers(soon(), []string{d.Sock})
			if code != c.want || len(list) != c.kept {
				t.Errorf("list %+v code %q, want %d and %q", list, code, c.kept, c.want)
			}
		})
	}
}

// No socket at any candidate is its own code, not a connect failure. Mutation: treating ENOENT
// like every other error says connect.
func TestNoSocketIsACode(t *testing.T) {
	dir := dockertest.SocketDir(t)
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
	dir := dockertest.SocketDir(t)
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
	d := engine(t, fakeContainer{"abc", "alpine", dockertest.Stats(1<<20, 2<<20, ``)})
	if list, _, code := Containers(soon(), []string{refused, d.Sock}); code != "" || len(list) != 1 {
		t.Errorf("next candidate: list %+v code %q", list, code)
	}
	if n := len(d.Requests()); n != 2 {
		t.Errorf("%d requests to the second candidate, want 2", n)
	}
}

// replaceConnect makes the recorder connect with fn; the test's end restores it.
func replaceConnect(t *testing.T, fn func(string) (int, string)) {
	t.Helper()
	saved := docker.Connect
	docker.Connect = fn
	t.Cleanup(func() { docker.Connect = saved })
}

// A DOCKER_HOST over TCP is a code, with no connection of any kind. Mutation: leaving the test out
// connects to the marker as a path.
func TestTCPDockerHost(t *testing.T) {
	var calls atomic.Int64
	replaceConnect(t, func(string) (int, string) { calls.Add(1); return -1, "connect" })
	if list, total, code := Containers(soon(), []string{docker.TCPHost}); code != "tcp_host" || total != 0 || list != nil {
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
	d := dockertest.Serve(t, func(net.Conn, string) { <-done })
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	start := time.Now()
	r.ContainersAsync([]string{d.Sock})
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
	if ms, _ := got[0]["read_ms"].(float64); ms < 1900 {
		t.Errorf("read_ms %v, want the deadline's 2 s", got[0]["read_ms"])
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
	d := engine(t, fakeContainer{longID, "postgres:16", dockertest.Stats(300<<20, 2048<<20, ``)})
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	r.MemoryAsync()
	r.memWG.Wait()
	r.ContainersAsync([]string{d.Sock})
	r.memWG.Wait()
	r.ContainersAsync([]string{d.Sock})
	r.memWG.Wait()
	got := lines(t, dir)
	if len(got) != 2 || d.Conns() != 2 {
		t.Fatalf("%d lines and %d connections, want 2 and 2: %v", len(got), d.Conns(), got)
	}
	l := got[1]
	if l["e"] != "containers" || l["run"] != nil || l["n"] != nil || l["containers_total"] != float64(1) {
		t.Errorf("line %v", l)
	}
	if _, has := l["containers_err"]; has {
		t.Errorf("a clean read has containers_err: %v", l)
	}
	if _, has := l["read_ms"]; !has {
		t.Errorf("no read_ms: %v", l)
	}
	c := l["containers"].([]any)[0].(map[string]any)
	if c["id"] != longID[:12] || c["image"] != "postgres:16" || c["usage_mib"] != float64(300) || c["limit_mib"] != float64(2048) {
		t.Errorf("container %v", c)
	}
	var nilRec *Recorder
	nilRec.ContainersAsync([]string{d.Sock})
	stopped := New(stateDir(t), "run-2", "x")
	stopped.Stop()
	stopped.ContainersAsync([]string{d.Sock})
	stopped.memWG.Wait()
	if n := d.Conns(); n != 2 {
		t.Errorf("%d connections, want none from a nil or stopped recorder", n-2)
	}
}

// The binary reaches the socket by raw syscalls alone. Mutation: importing net in the docker client.
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
