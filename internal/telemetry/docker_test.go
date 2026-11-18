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

// engine answers as a Docker daemon with these containers running.
func engine(t *testing.T, cs ...dockertest.Container) *dockertest.Fake {
	t.Helper()
	var opts []dockertest.Option
	for _, c := range cs {
		opts = append(opts, dockertest.WithContainer(c))
	}
	return dockertest.Engine(t, opts...)
}

// statsPath is the request target of a container's stats.
func statsPath(id string) string {
	path, _ := docker.StatsPath(id)
	return path
}

// The list, then one stats request per container and nothing else, each over HTTP/1.0 with the
// id cut to 12. Mutations: an id left whole reddens the list; a stats path without one-shot=true
// reddens the requests; limit taken from usage reddens the list.
func TestContainersParsesStats(t *testing.T) {
	d := engine(t,
		dockertest.Container{ID: dockertest.LongID, Image: "postgres:16", UsageBytes: 300 << 20, InactiveFileBytes: 100, LimitBytes: 2048 << 20},
		dockertest.Container{ID: dockertest.ID('c'), Image: "alpine", UsageBytes: 5 << 20, LimitBytes: 1024 << 20})
	list, total, code := Containers(dockertest.Soon(), []string{d.Sock})
	want := []docker.Container{{ID: dockertest.LongID[:12], Image: "postgres:16", UsageMiB: 299, LimitMiB: 2048}, {ID: dockertest.ID('c')[:12], Image: "alpine", UsageMiB: 5, LimitMiB: 1024}}
	if !reflect.DeepEqual(list, want) || total != 2 || code != "" {
		t.Errorf("list %+v total %d code %q, want %+v 2 and none", list, total, code, want)
	}
	reqs := d.Requests()
	wantReqs := []string{"GET /containers/json HTTP/1.0", "GET " + statsPath(dockertest.LongID) + " HTTP/1.0", "GET " + statsPath(dockertest.ID('c')) + " HTTP/1.0"}
	if !reflect.DeepEqual(reqs, wantReqs) {
		t.Errorf("requests %q, want %q", reqs, wantReqs)
	}
}

// A container that went between the list and its stats, answering with zeros (stopped) or 404
// (removed, as `docker run --rm` does), is counted in the total, left out of the list and is no
// error. Mutations: without the limit test the stopped one is listed at 0; a 404 read as any other
// status puts http on the line.
func TestGoneContainerLeftOut(t *testing.T) {
	d := dockertest.Engine(t, dockertest.StoppedAtStats(dockertest.ID('1')), dockertest.StatsStatus(dockertest.ID('2'), 404),
		dockertest.WithContainer(dockertest.Container{ID: dockertest.ID('1'), Image: "alpine"}), dockertest.WithContainer(dockertest.Container{ID: dockertest.ID('2'), Image: "alpine"}),
		dockertest.WithContainer(dockertest.Container{ID: dockertest.ID('c'), Image: "alpine", UsageBytes: 5 << 20, LimitBytes: 1024 << 20}))
	list, total, code := Containers(dockertest.Soon(), []string{d.Sock})
	if code != "" || total != 3 || len(list) != 1 || list[0].ID != dockertest.ID('c')[:12] {
		t.Errorf("list %v, total %d, code %q; want only the third of 3, no code", list, total, code)
	}
}

// An id that is not 64 hexadecimal characters is put in no request: its container costs only itself,
// with the parse code. Mutation: building the path from any id sends the first container's request.
func TestOddIDCostsOnlyItself(t *testing.T) {
	d := engine(t, dockertest.Container{ID: "abc d", Image: "a", UsageBytes: 1 << 20, LimitBytes: 1 << 20},
		dockertest.Container{ID: dockertest.ID('c'), Image: "b", UsageBytes: 1 << 20, LimitBytes: 1 << 20})
	list, total, code := Containers(dockertest.Soon(), []string{d.Sock})
	if code != "parse" || total != 2 || len(list) != 1 || list[0].ID != dockertest.ID('c')[:12] || len(d.Requests()) != 2 {
		t.Errorf("list %+v total %d code %q after %q, want the second only, 2, parse and one stats request", list, total, code, d.Requests())
	}
}

// Sixteen are read and all are counted, and the longest line of sixteen fits. Mutations: no cap
// makes 21 requests; total taken after the cap says 16; an image left at full length makes the
// line too long, so nothing is written.
func TestContainersCappedAtSixteen(t *testing.T) {
	var cs []dockertest.Container
	image := strings.Repeat("i", 200)
	for i := range 20 {
		cs = append(cs, dockertest.Container{ID: fmt.Sprintf("%064d", i), Image: image, UsageBytes: 1 << 40, LimitBytes: 1 << 40})
	}
	d := engine(t, cs...)
	list, total, code := Containers(dockertest.Soon(), []string{d.Sock})
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
	r.ContainersAsync([]string{d.Sock}, docker.On)
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
			dockertest.Reply(conn, 200, `[{"Id":"`+dockertest.ID('a')+`","Image":"a"},{"Id":"`+dockertest.ID('b')+`","Image":"b"}]`)
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

// A container whose stats are junk costs itself, not the ones after it. (The list's junk is the
// docker package's.) Mutation: a failed stats ending the loop loses the row's good container.
func TestJunkStatsCostOnlyTheContainer(t *testing.T) {
	good := dockertest.Stats(2<<20, 4<<20, ``)
	for _, c := range []struct{ name, stats string }{
		{"stats is junk", "{"},
		{"stats is the wrong type", `{"memory_stats":{"usage":"lots"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := dockertest.Serve(t, func(conn net.Conn, target string) {
				switch target {
				case "/containers/json":
					dockertest.Reply(conn, 200, `[{"Id":"`+dockertest.ID('b')+`","Image":"b"},{"Id":"`+dockertest.ID('e')+`","Image":"good"}]`)
				case statsPath(dockertest.ID('b')):
					dockertest.Reply(conn, 200, c.stats)
				default:
					dockertest.Reply(conn, 200, good)
				}
			})
			list, _, code := Containers(dockertest.Soon(), []string{d.Sock})
			if code != "parse" || len(list) != 1 {
				t.Errorf("list %+v code %q, want one and parse", list, code)
			}
		})
	}
}

// replaceConnect makes the recorder connect with fn; the test's end restores it.
func replaceConnect(t *testing.T, fn func(string) (int, string)) {
	t.Helper()
	saved := docker.Connect
	docker.Connect = fn
	t.Cleanup(func() { docker.Connect = saved })
}

// A DOCKER_HOST over TCP is the code tcp_host on a line, with no connection of any kind; any other
// reason the features are off is no line. Mutations: reading the sockets whatever the gate says
// connects; dropping the remote-host row's code leaves no containers_err.
func TestContainersLineByGate(t *testing.T) {
	var calls atomic.Int64
	replaceConnect(t, func(string) (int, string) { calls.Add(1); return -1, "connect" })
	for _, c := range []struct {
		off  docker.Off
		want int
	}{{docker.OffRemoteHost, 1}, {docker.OffNested, 0}, {docker.OffConfig, 0}} {
		dir := stateDir(t)
		r := New(dir, "run-1", "x")
		r.ContainersAsync([]string{"/nonexistent"}, c.off)
		r.memWG.Wait()
		if _, err := os.Stat(eventsPath(dir)); c.want == 0 {
			if err == nil {
				t.Errorf("off %q: a line was written", c.off)
			}
		} else if got := lines(t, dir); len(got) != 1 || got[0]["containers_err"] != "tcp_host" || got[0]["containers_total"] != float64(0) {
			t.Errorf("off %q: lines %v, want one with tcp_host", c.off, got)
		}
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
	r.ContainersAsync([]string{d.Sock}, docker.On)
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
	d := engine(t, dockertest.Container{ID: dockertest.LongID, Image: "postgres:16", UsageBytes: 300 << 20, LimitBytes: 2048 << 20})
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	r.MemoryAsync()
	r.memWG.Wait()
	r.ContainersAsync([]string{d.Sock}, docker.On)
	r.memWG.Wait()
	r.ContainersAsync([]string{d.Sock}, docker.On)
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
	if c["id"] != dockertest.LongID[:12] || c["image"] != "postgres:16" || c["usage_mib"] != float64(300) || c["limit_mib"] != float64(2048) {
		t.Errorf("container %v", c)
	}
	var nilRec *Recorder
	nilRec.ContainersAsync([]string{d.Sock}, docker.On)
	stopped := New(stateDir(t), "run-2", "x")
	stopped.Stop()
	stopped.ContainersAsync([]string{d.Sock}, docker.On)
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
