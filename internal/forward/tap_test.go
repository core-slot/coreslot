//go:build unix

package forward

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/docker/dockertest"
)

// finish ends the forwarder once the test's connections are closed and returns every event after
// ready, in order.
func finish(t *testing.T, s *served, clients ...*http.Client) []string {
	t.Helper()
	for _, c := range clients {
		c.CloseIdleConnections()
	}
	s.end.Close()
	select {
	case err := <-s.done:
		s.done <- err // for the cleanup
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	var lines []string
	for {
		line, err := s.events.ReadString('\n')
		if err != nil {
			return lines
		}
		lines = append(lines, strings.TrimSuffix(line, "\n"))
	}
}

// sameEvents compares the events of several connections, whose order among them is not kept.
func sameEvents(got, want []string) bool {
	return reflect.DeepEqual(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want)))
}

// createdID creates a container through c and returns its id.
func createdID(t *testing.T, c *http.Client, target string) string {
	t.Helper()
	st, b := must(t, c, "POST", target, []byte(`{"Image":"alpine"}`))
	var out struct{ Id string }
	if st != "201 Created" || json.Unmarshal(b, &out) != nil {
		t.Fatalf("create: %s %s", st, b)
	}
	return out.Id
}

// A create answered 201 is reported with the id of the answer; one answered 404 is not.
// Mutation: reporting the create at its request reddens the 404 case.
func TestTapCreatedFromAnswer(t *testing.T) {
	d := dockertest.HTTP(t)
	s := serve(t, d.Sock)
	c := client(t, s.addr)
	id := createdID(t, c, "/v1.47/containers/create")
	if got, want := finish(t, s, c), []string{"connected", "created " + id}; !reflect.DeepEqual(got, want) {
		t.Errorf("events %q, want %q", got, want)
	}
	missing := dockertest.HTTP(t, dockertest.MissingImage())
	s = serve(t, missing.Sock)
	c = client(t, s.addr)
	if st, _ := must(t, c, "POST", "/v1.47/containers/create", []byte(`{"Image":"alpine"}`)); st != "404 Not Found" {
		t.Fatalf("create of a missing image: %s", st)
	}
	if got, want := finish(t, s, c), []string{"connected"}; !reflect.DeepEqual(got, want) {
		t.Errorf("events %q, want %q", got, want)
	}
}

// A start, a restart and an exec answered 2xx are reported with the reference as the client wrote
// it, the version prefix removed, whatever connection they come on; an answer of 404 reports
// nothing, and the forwarder says connected once. Mutation: a touch reported at its request
// reddens the 404.
func TestTapTouched(t *testing.T) {
	d := dockertest.HTTP(t)
	s := serve(t, d.Sock)
	c1, c2 := client(t, s.addr), client(t, s.addr)
	id := createdID(t, c1, "/containers/create?name=web")
	must(t, c1, "POST", "/v1.47/containers/web/start", nil)
	must(t, c2, "POST", "/v1.47/containers/"+id[:12]+"/restart", nil)
	must(t, c1, "POST", "/v1.47/containers/"+id+"/exec", []byte(`{"Cmd":["true"]}`))
	if st, _ := must(t, c2, "POST", "/v1.47/containers/nope/start", nil); st != "404 Not Found" {
		t.Fatalf("start of an unknown container: %s", st)
	}
	want := []string{"connected", "created " + id, "touched web", "touched " + id[:12], "touched " + id}
	if got := finish(t, s, c1, c2); !sameEvents(got, want) {
		t.Errorf("events %q, want %q", got, want)
	}
}

// An interim 100 Continue before each answer is skipped when answers are paired with requests.
// Mutation: pairing the 100 with the create loses the created event.
func TestTapPairsAcrossContinue(t *testing.T) {
	d := dockertest.HTTP(t, dockertest.Continue())
	s := serve(t, d.Sock)
	c := client(t, s.addr)
	id := createdID(t, c, "/v1.47/containers/create?name=x")
	must(t, c, "POST", "/v1.47/containers/x/start", nil)
	if got, want := finish(t, s, c), []string{"connected", "created " + id, "touched x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("events %q, want %q", got, want)
	}
}

// After an exec start's 101 the stream holds no event: bytes that look like a create and a start,
// both ways, report nothing and make nothing blind. Mutation: a tap that parses past the 101 goes
// blind on the echo.
func TestTapStopsAtUpgrade(t *testing.T) {
	d := dockertest.HTTP(t)
	s := serve(t, d.Sock)
	conn, br := upgraded(t, d, s.addr)
	msg := "POST /containers/create HTTP/1.1\r\nContent-Length: 0\r\n\r\nPOST /containers/u/start HTTP/1.1\r\nContent-Length: 0\r\n\r\n"
	conn.Write([]byte(msg))
	conn.(*net.UnixConn).CloseWrite()
	if got, _ := io.ReadAll(br); string(got) != msg {
		t.Fatalf("echo %q", got)
	}
	conn.Close()
	got := finish(t, s)
	if len(got) != 4 || got[0] != "connected" || !strings.HasPrefix(got[1], "created ") || got[2] != "touched u" || got[3] != "touched u" {
		t.Errorf("events %q, want connected, the create and two touches only", got)
	}
}

// An exec start or an attach answered 200 with a stream's content type takes the connection over as
// a 101 does: what the client writes after it reports nothing and makes nothing blind.
// Mutation: recognising only 101 parses the client's stdin as a request and goes blind.
func TestTapStopsAtHijackWithout101(t *testing.T) {
	d := dockertest.HTTP(t)
	s := serve(t, d.Sock)
	c := client(t, s.addr)
	createdID(t, c, "/containers/create?name=h")
	must(t, c, "POST", "/containers/h/start", nil)
	_, b := must(t, c, "POST", "/containers/h/exec", []byte(`{"Cmd":["cat"]}`))
	execID := string(b[7:71])
	for _, target := range []string{"/v1.47/exec/" + execID + "/start", "/v1.47/containers/h/attach?stream=1&stdin=1&stdout=1"} {
		conn, err := net.Dial("unix", s.addr)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: docker\r\nContent-Length: 2\r\n\r\n{}", target)
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		if err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != dockertest.RawStream {
			t.Fatalf("%s: %v, %v", target, resp, err)
		}
		stdin := "\x01\x00\x00\x00\x00\x00\x00\x05hello GET / HTTP/1.1\r\n\r\n"
		conn.Write([]byte(stdin))
		conn.(*net.UnixConn).CloseWrite()
		if got, err := io.ReadAll(resp.Body); err != nil || string(got) != stdin {
			t.Errorf("%s: echo %q, %v", target, got, err)
		}
		conn.Close()
	}
	got := finish(t, s, c)
	if len(got) != 4 || got[2] != "touched h" || got[3] != "touched h" {
		t.Errorf("events %q, want connected, the create and two touches only", got)
	}
}

// Bytes that are not HTTP make the tap blind, a request's and an answer's alike, and the relay still
// passes them. Mutation: a parser that ends quietly on a head that does not parse reports no blind
// for the answer.
func TestTapBlindOnGarbage(t *testing.T) {
	d := dockertest.HTTP(t)
	s := serve(t, d.Sock)
	conn, err := net.Dial("unix", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	conn.Write([]byte("NOT HTTP AT ALL\r\n\r\n"))
	if resp, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil || resp.StatusCode != 400 {
		t.Errorf("the engine's answer: %v, %v; want its 400", resp, err)
	}
	conn.Close()
	if got, want := finish(t, s), []string{"connected", "blind"}; !reflect.DeepEqual(got, want) {
		t.Errorf("a request that is not HTTP: events %q, want %q", got, want)
	}
	odd := dockertest.Serve(t, func(c net.Conn, _ string) { io.WriteString(c, "NOT HTTP EITHER\r\n\r\n") })
	s = serve(t, odd.Sock)
	conn, err = net.Dial("unix", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "GET /info HTTP/1.1\r\nHost: docker\r\n\r\n")
	if got, _ := io.ReadAll(conn); string(got) != "NOT HTTP EITHER\r\n\r\n" {
		t.Errorf("the answer through the forwarder: %q", got)
	}
	conn.Close()
	if got, want := finish(t, s), []string{"connected", "blind"}; !reflect.DeepEqual(got, want) {
		t.Errorf("an answer that is not HTTP: events %q, want %q", got, want)
	}
}

// putWithHead sends an archive put whose head is exactly size bytes and returns the status.
func putWithHead(t *testing.T, addr string, size int, body []byte) int {
	t.Helper()
	conn, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	head := fmt.Sprintf("PUT /containers/a/archive?path=/%d HTTP/1.1\r\nHost: docker\r\nContent-Length: %d\r\nX-Pad: ", size, len(body))
	head += strings.Repeat("p", size-len(head)-4) + "\r\n\r\n"
	if len(head) != size {
		t.Fatalf("head of %d bytes, want %d", len(head), size)
	}
	conn.Write(append([]byte(head), body...))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("answer to a %d-byte head: %v", size, err)
	}
	return resp.StatusCode
}

// A head of MaxHead bytes is read; one byte more makes the tap blind, and the bytes still reach the
// engine intact. Mutation: a limit of MaxHead+1 reports no blind.
func TestTapBlindOnOversizedHead(t *testing.T) {
	d := dockertest.HTTP(t)
	s := serve(t, d.Sock)
	c := client(t, s.addr)
	id := createdID(t, c, "/containers/create?name=a")
	body := make([]byte, 1<<20)
	rand.Read(body)
	for _, size := range []int{MaxHead, MaxHead + 1} {
		if code := putWithHead(t, s.addr, size, body); code != 200 {
			t.Errorf("a %d-byte head: %d, want 200", size, code)
		}
		if _, got := must(t, client(t, d.Sock), "GET", fmt.Sprintf("/containers/a/archive?path=/%d", size), nil); !bytes.Equal(got, body) {
			t.Errorf("after a %d-byte head the engine holds %d bytes of %d", size, len(got), len(body))
		}
	}
	if got, want := finish(t, s, c), []string{"connected", "created " + id, "blind"}; !sameEvents(got, want) {
		t.Errorf("events %q, want %q", got, want)
	}
}

// A tap that falls behind by more than its buffer is blind, and the relay passes every byte.
// Mutation: blocking the relay on a full buffer stalls the transfer until the parsers resume.
func TestTapBlindWhenBehind(t *testing.T) {
	d := dockertest.HTTP(t)
	big := make([]byte, 16<<20+512<<10)
	rand.Read(big)
	direct := client(t, d.Sock)
	must(t, direct, "POST", "/containers/create?name=b", []byte(`{"Image":"alpine"}`))
	must(t, direct, "PUT", "/containers/b/archive?path=/x", big)
	release := make(chan struct{})
	parseHold = func() { <-release }
	t.Cleanup(func() { parseHold = func() {} })
	s := serve(t, d.Sock)
	c := client(t, s.addr)
	c.Timeout = 10 * time.Second
	st, got, err := do(c, "GET", "/containers/b/archive?path=/x", nil)
	close(release)
	if err != nil || st != "200 OK" || !bytes.Equal(got, big) {
		t.Errorf("through a stalled tap: %q, %d bytes of %d, %v", st, len(got), len(big), err)
	}
	if got, want := finish(t, s, c), []string{"connected", "blind"}; !reflect.DeepEqual(got, want) {
		t.Errorf("events %q, want %q", got, want)
	}
}

// Two requests on one connection, the first with a chunked body, are both seen.
// Mutation: a tap that stops after the first message on a connection misses the start.
func TestTapKeepAliveChunked(t *testing.T) {
	d := dockertest.HTTP(t)
	s := serve(t, d.Sock)
	conn, err := net.Dial("unix", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	body := `{"Image":"alpine"}`
	fmt.Fprintf(conn, "POST /v1.47/containers/create?name=k HTTP/1.1\r\nHost: docker\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n%x\r\n%s\r\n0\r\n\r\n", 5, body[:5], len(body)-5, body[5:])
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("create: %v, %v", resp, err)
	}
	var out struct{ Id string }
	json.NewDecoder(resp.Body).Decode(&out)
	io.Copy(io.Discard, resp.Body)
	fmt.Fprintf(conn, "POST /v1.47/containers/k/start HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\n\r\n")
	if resp, err := http.ReadResponse(br, nil); err != nil || resp.StatusCode != 204 {
		t.Fatalf("start: %v, %v", resp, err)
	}
	conn.Close()
	if got, want := finish(t, s), []string{"connected", "created " + out.Id, "touched k"}; !reflect.DeepEqual(got, want) {
		t.Errorf("events %q, want %q", got, want)
	}
}

// A line is at most 256 bytes with its LF; a ref that would pass that, or that would break the
// line, makes the line blind. Mutation: a bound of 257 keeps the longer ref.
func TestEventLineBound(t *testing.T) {
	fits := strings.Repeat("r", 256-len("touched \n"))
	if l := (Event{KindTouched, fits}).Line(); len(l) != 256 || l != "touched "+fits+"\n" {
		t.Errorf("a ref that fits: %d bytes %q", len(l), l)
	}
	for _, ref := range []string{fits + "r", "a b", "a\nb", "a\rb"} {
		if l := (Event{KindTouched, ref}).Line(); l != "blind\n" {
			t.Errorf("ref %q: line %q, want blind", ref, l)
		}
	}
	if l := (Event{Kind: KindConnected}).Line(); l != "connected\n" {
		t.Errorf("connected: %q", l)
	}
}

// ParseEvent takes the lines Line writes and nothing else. Mutation: accepting any kind lets
// "unknown x" through.
func TestParseEventRejects(t *testing.T) {
	for _, line := range []string{"", "\n", " x", "x", "unknown x", "created", "created ", "touched a b", "ready x", "blind x", "connected x\n",
		"touched " + strings.Repeat("r", 248) + "\n"} {
		if e, ok := ParseEvent(line); ok {
			t.Errorf("%q parsed as %+v", line, e)
		}
	}
	for _, e := range []Event{{KindReady, ""}, {KindConnected, ""}, {KindBlind, ""}, {KindCreated, dockertest.LongID}, {KindTouched, "web-1"}} {
		if got, ok := ParseEvent(e.Line()); !ok || got != e {
			t.Errorf("%q: %+v, %v; want %+v", e.Line(), got, ok, e)
		}
	}
}
