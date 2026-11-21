//go:build unix

package forward

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/docker/dockertest"
)

// client is an HTTP client whose every connection goes to the unix socket sock; a test's cleanup
// closes its idle connections, which a draining forwarder waits for.
func client(t *testing.T, sock string) *http.Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// do makes one request and returns its status line and body, or the error.
func do(c *http.Client, method, target string, body []byte) (string, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://docker"+target, rd)
	if err != nil {
		return "", nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.Status, b, err
}

// must is do that fails the test on an error.
func must(t *testing.T, c *http.Client, method, target string, body []byte) (string, []byte) {
	t.Helper()
	status, b, err := do(c, method, target, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	return status, b
}

// served is a forwarder serving in the test's goroutine: its address, the write end of End, its
// events and its result.
type served struct {
	entry, addr string
	end         *os.File
	events      *bufio.Reader
	done        chan error
}

// serve starts Serve on an entry of a fresh directory, relaying to upstream, and waits for ready.
func serve(t *testing.T, upstream string) *served {
	t.Helper()
	parent := filepath.Join(dockertest.SocketDir(t), "fw")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	endR, endW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	evR, evW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	s := &served{entry: filepath.Join(parent, "1-abc"), end: endW, events: bufio.NewReader(evR), done: make(chan error, 1)}
	s.addr = filepath.Join(s.entry, SockName)
	go func() {
		s.done <- Serve(Config{Entry: s.entry, Upstream: upstream, End: endR, Events: evW})
		endR.Close()
		evW.Close()
	}()
	t.Cleanup(func() {
		endW.Close()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after the test")
		}
		evR.Close()
	})
	evR.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := s.events.ReadString('\n'); line != "ready\n" {
		t.Fatalf("first event %q, %v; want ready", line, err)
	}
	evR.SetReadDeadline(time.Time{})
	return s
}

// The relay passes every route's answer as the engine gives it: create, start, inspect, logs, 16.5 MiB
// of archive each way and the events stream are the same through the forwarder as direct.
// Mutation: a copy that drops its last partial buffer reddens the archive.
func TestRelaySameBytesBothWays(t *testing.T) {
	d := dockertest.HTTP(t)
	s := serve(t, d.Sock)
	via, direct := client(t, s.addr), client(t, d.Sock)
	if st, _ := must(t, via, "POST", "/v1.47/containers/create?name=a", []byte(`{"Image":"alpine"}`)); st != "201 Created" {
		t.Fatalf("create through the forwarder: %s", st)
	}
	if st, _ := must(t, direct, "POST", "/v1.47/containers/create?name=b", []byte(`{"Image":"alpine"}`)); st != "201 Created" {
		t.Fatalf("create direct: %s", st)
	}
	big := make([]byte, 16<<20+512<<10)
	rand.Read(big)
	must(t, via, "PUT", "/containers/a/archive?path=/in", big)
	must(t, direct, "PUT", "/containers/a/archive?path=/out", big)
	for _, call := range []struct{ method, target string }{
		{"POST", "/containers/a/start"},
		{"GET", "/v1.47/containers/a/json"},
		{"GET", "/containers/a/logs?stdout=1&stderr=1"},
		{"GET", "/containers/a/archive?path=/in"},
		{"GET", "/containers/a/archive?path=/out"},
		{"POST", "/containers/b/start"},
	} {
		c := via
		if call.method == "POST" {
			c = direct // a start changes the container; the second of a pair would answer 304
		}
		st1, b1 := must(t, c, call.method, call.target, nil)
		if call.method == "POST" {
			c = via
			call.target = strings.Replace(call.target, "start", "stop", 1)
		} else {
			c = direct
		}
		st2, b2 := must(t, c, call.method, call.target, nil)
		if st1 != st2 || !bytes.Equal(b1, b2) {
			t.Errorf("%s %s: %s with %d bytes, then %s with %d bytes", call.method, call.target, st1, len(b1), st2, len(b2))
		}
	}
	if _, b := must(t, direct, "GET", "/containers/a/archive?path=/in", nil); !bytes.Equal(b, big) {
		t.Errorf("the archive put through the forwarder came back with %d bytes of %d", len(b), len(big))
	}
	firstEvent := func(c *http.Client) string {
		resp, err := c.Get("http://docker/v1.47/events")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		return resp.Status + " " + line
	}
	if e1, e2 := firstEvent(via), firstEvent(direct); e1 != e2 || !strings.HasPrefix(e1, "200 OK {") {
		t.Errorf("events: %q through the forwarder, %q direct", e1, e2)
	}
}

// A client that shuts its write side after its request still reads the whole answer.
// Mutation: closing both connections at the first end of input truncates the answer.
func TestRelayHalfClose(t *testing.T) {
	d := dockertest.HTTP(t)
	s := serve(t, d.Sock)
	big := make([]byte, 1<<20)
	rand.Read(big)
	c := client(t, s.addr)
	must(t, c, "POST", "/containers/create?name=h", []byte(`{"Image":"alpine"}`))
	must(t, c, "PUT", "/containers/h/archive?path=/x", big)
	conn, err := net.Dial("unix", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /containers/h/archive?path=/x HTTP/1.1\r\nHost: docker\r\n\r\n")
	conn.(*net.UnixConn).CloseWrite()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(resp.Body); err != nil || !bytes.Equal(b, big) {
		t.Errorf("after the half-close: %d bytes of %d, %v", len(b), len(big), err)
	}
}

// upgraded returns a connection through addr whose exec start the engine answered 101.
func upgraded(t *testing.T, d *dockertest.Fake, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c := client(t, addr)
	must(t, c, "POST", "/containers/create?name=u", []byte(`{"Image":"alpine"}`))
	must(t, c, "POST", "/containers/u/start", nil)
	_, b := must(t, c, "POST", "/containers/u/exec", []byte(`{"Cmd":["cat"]}`))
	id := string(b[7:71]) // {"Id":"<64 hex>"}
	c.CloseIdleConnections()
	conn, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "POST /v1.47/exec/%s/start HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 2\r\n\r\n{}", id)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("exec start: %v, %v", resp, err)
	}
	return conn, br
}

// After a 101 both directions copy raw bytes: what looks like HTTP and what does not comes back
// intact. Mutation: a relay that parses past the upgrade loses the bytes.
func TestRelayUpgradeIsRaw(t *testing.T) {
	d := dockertest.HTTP(t)
	s := serve(t, d.Sock)
	conn, br := upgraded(t, d, s.addr)
	msg := "POST /containers/create HTTP/1.1\r\nContent-Length: 0\r\n\r\n\x01\x00\x00\x00\x00\x00\x00\x03abc\xff"
	conn.Write([]byte(msg))
	conn.(*net.UnixConn).CloseWrite()
	if got, err := io.ReadAll(br); err != nil || string(got) != msg {
		t.Errorf("echo %q, %v; want %q", got, err, msg)
	}
}

// Fifty calls at once all succeed, and the relay's goroutines end with their connections.
// Mutation: a connection whose goroutines never return leaves the count above its baseline.
func TestRelayFiftyParallel(t *testing.T) {
	d := dockertest.HTTP(t, dockertest.WithContainer(dockertest.Container{ID: dockertest.LongID, Name: "db", Image: "postgres"}))
	s := serve(t, d.Sock)
	base := runtime.NumGoroutine()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", s.addr)
	}, MaxIdleConnsPerHost: 50}
	c := &http.Client{Transport: tr}
	var wg sync.WaitGroup
	var failed atomic.Int64
	for i := range 50 {
		wg.Go(func() {
			target := "/containers/json"
			if i%2 == 0 {
				target = "/containers/db/json"
			}
			if st, _, err := do(c, "GET", target, nil); err != nil || st != "200 OK" {
				failed.Add(1)
			}
		})
	}
	wg.Wait()
	tr.CloseIdleConnections()
	if n := failed.Load(); n != 0 {
		t.Errorf("%d of 50 calls failed", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Errorf("%d goroutines after the calls, %d before", n, base)
	}
}

// flaky fails its first accepts.
type flaky struct {
	net.Listener
	fails atomic.Int64
}

func (f *flaky) Accept() (net.Conn, error) {
	if f.fails.Add(-1) >= 0 {
		return nil, errors.New("accept: too many open files")
	}
	return f.Listener.Accept()
}

// A failed accept is retried and never ends the forwarder: the next client is served.
// Mutation: returning from the accept loop on any error leaves the client unanswered.
func TestAcceptErrorNeverEnds(t *testing.T) {
	d := dockertest.HTTP(t)
	sock := filepath.Join(dockertest.SocketDir(t), "f.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &flaky{Listener: ln}
	f.fails.Store(3)
	r := newRelay(f, d.Sock, NewTap(io.Discard))
	go r.accept()
	t.Cleanup(r.stop) // after the client's idle connections close
	c := client(t, sock)
	c.Timeout = 5 * time.Second
	if st, _, err := do(c, "GET", "/info", nil); err != nil || st != "200 OK" {
		t.Errorf("after three failed accepts: %q, %v", st, err)
	}
}

// Closing End while an events stream is open swaps the address for a symlink to the upstream: new
// connections reach the engine through it, the open stream keeps flowing, and Serve returns when it
// closes. Mutation: returning at the swap without waiting for the open connections.
func TestEndSwapsAndDrains(t *testing.T) {
	d := dockertest.HTTP(t)
	s := serve(t, d.Sock)
	stream, err := client(t, s.addr).Get("http://docker/events")
	if err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(stream.Body)
	lines.ReadString('\n')
	s.end.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fi, err := os.Lstat(s.addr); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the address is not a symlink 5 s after End")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if target, _ := os.Readlink(s.addr); target != d.Sock {
		t.Errorf("the address points at %q, want %q", target, d.Sock)
	}
	before := d.Conns()
	if st, _ := must(t, client(t, s.addr), "GET", "/info", nil); st != "200 OK" || d.Conns() != before+1 {
		t.Errorf("a call after the swap: %s, %d new connections at the engine", st, d.Conns()-before)
	}
	if line, err := lines.ReadString('\n'); err != nil || !strings.Contains(line, `"seq":2`) {
		t.Errorf("the open stream after the swap: %q, %v", line, err)
	}
	select {
	case err := <-s.done:
		t.Fatalf("Serve returned %v with a stream open", err)
	default:
	}
	stream.Body.Close()
	select {
	case err := <-s.done:
		s.done <- err // for the cleanup
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Serve did not return 5 s after the last connection closed")
	}
}

// Two thousand calls across the swap, on connections old and new, none fails.
// Mutation: closing the listener at the swap without accepting what is queued fails a call that
// connected just before it, in about one run of twenty.
func TestSwapUnderLoad(t *testing.T) {
	d := dockertest.HTTP(t)
	s := serve(t, d.Sock)
	c := client(t, s.addr)
	var wg sync.WaitGroup
	var calls, failed atomic.Int64
	var once sync.Once
	for range 20 {
		wg.Go(func() {
			for range 100 {
				if calls.Add(1) == 1000 {
					once.Do(func() { s.end.Close() })
				}
				if st, _, err := do(c, "GET", "/info", nil); err != nil || st != "200 OK" {
					failed.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if n := failed.Load(); n != 0 {
		t.Errorf("%d of %d calls failed", n, calls.Load())
	}
	if fi, err := os.Lstat(s.addr); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the address after the run: %v, %v; want a symlink", fi, err)
	}
}
