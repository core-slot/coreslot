//go:build unix

package dockertest_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
)

// client is an HTTP client whose every connection goes to the unix socket sock.
func client(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
}

// call makes one request and returns its status and body.
func call(t *testing.T, c *http.Client, method, target string, body io.Reader) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://docker"+target, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: body: %v", method, target, err)
	}
	return resp.StatusCode, b
}

// create makes a container of the image with the name and returns its id.
func create(t *testing.T, c *http.Client, name string) string {
	t.Helper()
	code, b := call(t, c, "POST", "/v1.47/containers/create?name="+name, strings.NewReader(`{"Image":"alpine","Cmd":["true"]}`))
	var out struct{ Id string }
	if code != 201 || json.Unmarshal(b, &out) != nil || len(out.Id) != 64 {
		t.Fatalf("create: %d %s", code, b)
	}
	return out.Id
}

// rawConn dials the socket and returns the connection with a reader of its answers.
func rawConn(t *testing.T, sock string) (*net.UnixConn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(10 * time.Second))
	return c.(*net.UnixConn), bufio.NewReader(c)
}

// Two requests on one connection are both answered, and the fake records both with the method.
// Mutation: a server that closes after each answer reddens the second read.
func TestHTTPKeepAliveTwoRequests(t *testing.T) {
	d := dockertest.HTTP(t, dockertest.WithInfo("Docker Desktop", 4<<30))
	c, br := rawConn(t, d.Sock)
	for _, target := range []string{"/info", "/v1.47/info"} {
		fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: docker\r\n\r\n", target)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(b), `"MemTotal":4294967296`) {
			t.Errorf("%s: %d %s", target, resp.StatusCode, b)
		}
	}
	if got, want := d.Requests(), []string{"GET /info", "GET /v1.47/info"}; !reflect.DeepEqual(got, want) || d.Conns() != 1 {
		t.Errorf("requests %q over %d connections, want %q over 1", got, d.Conns(), want)
	}
}

// A chunked request body is read: the labels of a create sent in chunks are the container's, and
// the connection serves the next request. Mutation: ignoring the body leaves the labels out.
func TestHTTPReadsChunkedBody(t *testing.T) {
	d := dockertest.HTTP(t)
	c, br := rawConn(t, d.Sock)
	body := `{"Image":"alpine","Labels":{"coreslot.check":""}}`
	fmt.Fprintf(c, "POST /v1.47/containers/create?name=chunky HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n")
	fmt.Fprintf(c, "%x\r\n%s\r\n%x\r\n%s\r\n0\r\n\r\n", 10, body[:10], len(body)-10, body[10:])
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("create: %v %v", resp, err)
	}
	io.Copy(io.Discard, resp.Body)
	fmt.Fprintf(c, "GET /containers/json?all=1&filters=%s HTTP/1.1\r\nHost: docker\r\n\r\n", `%7B%22label%22%3A%5B%22coreslot.check%22%5D%7D`)
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	var list []docker.Listed
	if json.Unmarshal(b, &list) != nil || len(list) != 1 || list[0].Names[0] != "/chunky" {
		t.Errorf("list by label: %d %s", resp.StatusCode, b)
	}
}

// execStart makes a running container with an exec and returns a connection that has sent the
// exec's start, with or without asking for the upgrade.
func execStart(t *testing.T, d *dockertest.Fake, upgrade bool) (*net.UnixConn, *bufio.Reader) {
	t.Helper()
	cl := client(d.Sock)
	id := create(t, cl, "e")
	if code, b := call(t, cl, "POST", "/containers/e/start", nil); code != 204 {
		t.Fatalf("start: %d %s", code, b)
	}
	code, b := call(t, cl, "POST", "/v1.47/containers/"+id[:12]+"/exec", strings.NewReader(`{"Cmd":["cat"],"AttachStdin":true,"AttachStdout":true}`))
	var ex struct{ Id string }
	if code != 201 || json.Unmarshal(b, &ex) != nil || len(ex.Id) != 64 {
		t.Fatalf("exec create: %d %s", code, b)
	}
	c, br := rawConn(t, d.Sock)
	body := `{"Detach":false,"Tty":false}`
	head := ""
	if upgrade {
		head = "Connection: Upgrade\r\nUpgrade: tcp\r\n"
	}
	fmt.Fprintf(c, "POST /v1.47/exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\n%sContent-Length: %d\r\n\r\n%s", ex.Id, head, len(body), body)
	return c, br
}

// With Upgrade an exec start answers 101 and then echoes raw bytes until the client half-closes.
// Mutation: answering 200 to an upgrade reddens the status.
func TestHTTPExecStartUpgrade(t *testing.T) {
	c, br := execStart(t, dockertest.HTTP(t), true)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 || resp.Header.Get("Content-Type") != dockertest.MultiplexedStream || resp.Header.Get("Upgrade") != "tcp" {
		t.Fatalf("answer %v, %v", resp, err)
	}
	msg := "GET /info HTTP/1.1\r\n\r\n\x00\x01raw"
	c.Write([]byte(msg))
	c.CloseWrite()
	got, err := io.ReadAll(br)
	if err != nil || string(got) != msg {
		t.Errorf("echo %q, %v; want %q", got, err, msg)
	}
}

// Without Upgrade an exec start answers 200 with the raw-stream type and the echo in chunks, as the
// engine does. Mutation: a 101 here reddens the status.
func TestHTTPExecStartHijack200(t *testing.T) {
	c, br := execStart(t, dockertest.HTTP(t), false)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != dockertest.RawStream || len(resp.TransferEncoding) != 1 {
		t.Fatalf("answer %v, %v", resp, err)
	}
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q, %v", buf, err)
	}
	c.CloseWrite()
	if rest, err := io.ReadAll(resp.Body); err != nil || len(rest) != 0 {
		t.Errorf("after the half-close: %q, %v; want the last chunk", rest, err)
	}
}

// An inspect finds a container by its name, its 12-character id and its id, with and without the
// version prefix, and answers 404 for another name. Mutation: matching the id alone reddens the
// name.
func TestHTTPInspectByNameAndShortID(t *testing.T) {
	d := dockertest.HTTP(t)
	cl := client(d.Sock)
	id := create(t, cl, "web-1")
	for _, ref := range []string{"web-1", id[:12], id} {
		for _, prefix := range []string{"", "/v1.47"} {
			code, b := call(t, cl, "GET", prefix+"/containers/"+ref+"/json", nil)
			var out struct{ Id, Name string }
			if code != 200 || json.Unmarshal(b, &out) != nil || out.Id != id || out.Name != "/web-1" {
				t.Errorf("inspect %s%s: %d %s", prefix, ref, code, b)
			}
		}
	}
	if code, _ := call(t, cl, "GET", "/containers/web-2/json", nil); code != 404 {
		t.Errorf("inspect of another name: %d, want 404", code)
	}
}

// 16 MiB put into a container's path comes back whole. Mutation: storing a truncated body reddens
// the comparison.
func TestHTTPArchiveRoundTrip16MiB(t *testing.T) {
	d := dockertest.HTTP(t)
	cl := client(d.Sock)
	create(t, cl, "a")
	in := make([]byte, 16<<20)
	rand.Read(in)
	if code, b := call(t, cl, "PUT", "/v1.47/containers/a/archive?path=/tmp", bytes.NewReader(in)); code != 200 {
		t.Fatalf("put: %d %s", code, b)
	}
	code, out := call(t, cl, "GET", "/v1.47/containers/a/archive?path=/tmp", nil)
	if code != 200 || !bytes.Equal(in, out) {
		t.Errorf("get: %d, %d bytes back of %d", code, len(out), len(in))
	}
}

// The fake answers release 1's reading, and SetUsage moves a container's usage. Mutation: SetUsage
// that changes a copy leaves the second reading at the first's figure.
func TestHTTPReadingFollowsSetUsage(t *testing.T) {
	d := dockertest.HTTP(t, dockertest.WithContainer(dockertest.Container{ID: dockertest.LongID, Name: "db", Image: "postgres", UsageBytes: 100 << 20, LimitBytes: 1 << 30}))
	r := docker.Read([]string{d.Sock}, dockertest.Soon())
	if r.Code != "" || len(r.Containers) != 1 || r.Containers[0].UsageMiB != 100 {
		t.Fatalf("reading %+v", r)
	}
	d.SetUsage(dockertest.LongID, 300<<20)
	if r := docker.Read([]string{d.Sock}, dockertest.Soon()); len(r.Containers) != 1 || r.Containers[0].UsageMiB != 300 {
		t.Errorf("after SetUsage: %+v", r)
	}
}

// The stand-in answers both questions in the CLI's shapes and refuses anything else. Mutation: an
// endpoint left out of a line reddens the endpoints.
func TestCLIStandIn(t *testing.T) {
	endpoints := []string{"unix:///var/run/docker.sock", "tcp://192.0.2.10:2376"}
	dir := dockertest.CLI(t, endpoints, "docker-container")
	out, err := exec.Command(dir+"/docker", "context", "ls", "--format", "{{json .}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		var c map[string]any
		if err := json.Unmarshal([]byte(line), &c); err != nil || len(c) != 5 || c["Error"] != "" {
			t.Errorf("line %q: %v", line, err)
		}
		got = append(got, c["DockerEndpoint"].(string))
	}
	if !reflect.DeepEqual(got, endpoints) || !strings.Contains(string(out), `"Name":"default"`) {
		t.Errorf("context ls:\n%s", out)
	}
	out, err = exec.Command(dir+"/docker", "buildx", "inspect").Output()
	if err != nil || !strings.Contains(string(out), "\nDriver: docker-container\n") || !strings.HasPrefix(string(out), "Name:") {
		t.Errorf("buildx inspect: %v\n%s", err, out)
	}
	if err := exec.Command(dir+"/docker", "ps").Run(); err == nil {
		t.Error("docker ps succeeded, want a refusal")
	}
}
