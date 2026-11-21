//go:build unix

package dockertest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Content types of an answer that takes the connection over: the engine's for an attach or exec
// start answered 200 without an upgrade, and for one answered 101.
const (
	RawStream         = "application/vnd.docker.raw-stream"
	MultiplexedStream = "application/vnd.docker.multiplexed-stream"
)

// HTTPOption shapes the engine HTTP serves. It is Engine's Option: every option of Engine applies,
// and Continue, MissingImage and SlowWait apply to HTTP alone.
type HTTPOption = Option

// Continue makes every answer follow an interim 100 Continue.
func Continue() Option { return func(e *engine) { e.cont = true } }

// MissingImage makes every create answer 404, as an engine does for an image it does not hold; the
// fake never pulls.
func MissingImage() Option { return func(e *engine) { e.missing = true } }

// SlowWait makes a container's wait answer only after d.
func SlowWait(d time.Duration) Option { return func(e *engine) { e.slowWait = d } }

// hcontainer is a container of the HTTP engine.
type hcontainer struct {
	Container
	labels  map[string]string
	running bool
}

// httpEngine is the state behind HTTP's server.
type httpEngine struct {
	*engine
	fake *Fake

	mu       sync.Mutex
	list     []*hcontainer
	execs    map[string]*hcontainer
	archives map[string][]byte // by container id and path
	made     int               // ids made so far
	hijacked []net.Conn
}

// HTTP serves an engine over HTTP/1.1 on a unix socket under SocketDir: connections kept alive,
// request bodies read in either framing, every request recorded as its method and target. Each
// route answers with or without a version prefix (/v1.47/…), in the shapes Docker's engine gives;
// an attach or exec start takes the connection over and echoes what the client writes, after a 101
// when the request asks for the upgrade and after a 200 in chunks when it does not. A test's cleanup
// closes it.
func HTTP(t testing.TB, opts ...HTTPOption) *Fake {
	t.Helper()
	e := newEngine(opts)
	d := &Fake{Sock: filepath.Join(SocketDir(t), "docker.sock")}
	h := &httpEngine{engine: e, fake: d, execs: map[string]*hcontainer{}, archives: map[string][]byte{}}
	for _, c := range e.containers {
		h.list = append(h.list, &hcontainer{Container: c, labels: map[string]string{"com.docker.compose.project": c.Project}, running: true})
	}
	d.h = h
	ln, err := net.Listen("unix", d.Sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ConnState: func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			d.conns.Add(1)
		}
	}}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, c := range h.hijacked {
			c.Close()
		}
	})
	t.Cleanup(func() { close(e.done) }) // first: a hung handler returns before the server closes
	return d
}

// SetUsage sets the memory usage, in bytes, of the HTTP engine's container ref.
func (d *Fake) SetUsage(ref string, bytes int64) {
	d.h.mu.Lock()
	defer d.h.mu.Unlock()
	if c := d.h.find(ref); c != nil {
		c.UsageBytes = uint64(bytes)
	}
}

// record appends a request to the ones Requests returns.
func (d *Fake) record(r string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.targets = append(d.targets, r)
}

// versionPrefix is the API version a client may put before a route.
var versionPrefix = regexp.MustCompile(`^/v[0-9]+(\.[0-9]+)?/`)

// newID is an id as an engine makes one: 64 hexadecimal characters, no two alike in their first 12.
func (h *httpEngine) newID() string {
	h.made++
	sum := sha256.Sum256([]byte(strconv.Itoa(h.made)))
	return hex.EncodeToString(sum[:])
}

// find resolves a reference as the engine does: an id, a name, then a unique prefix of an id.
func (h *httpEngine) find(ref string) *hcontainer {
	for _, c := range h.list {
		if c.ID == ref {
			return c
		}
	}
	for _, c := range h.list {
		if c.Name == strings.TrimPrefix(ref, "/") {
			return c
		}
	}
	var found *hcontainer
	for _, c := range h.list {
		if ref != "" && strings.HasPrefix(c.ID, ref) {
			if found != nil {
				return nil
			}
			found = c
		}
	}
	return found
}

// answer writes a JSON answer.
func answer(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func (h *httpEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.fake.record(r.Method + " " + r.RequestURI)
	if h.hang {
		select {
		case <-h.done:
		case <-r.Context().Done():
		}
		return
	}
	time.Sleep(h.delay)
	if h.cont {
		w.WriteHeader(http.StatusContinue)
	}
	path := versionPrefix.ReplaceAllString(r.URL.Path, "/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == "GET" && path == "/info":
		answer(w, 200, fmt.Sprintf(`{"OperatingSystem":%q,"MemTotal":%d}`, h.os, h.memTotal))
	case r.Method == "GET" && path == "/containers/json":
		h.listContainers(w, r)
	case r.Method == "POST" && path == "/containers/create":
		h.create(w, r)
	case r.Method == "GET" && path == "/events":
		h.events(w, r)
	case len(parts) == 3 && parts[0] == "exec" && parts[2] == "start" && r.Method == "POST":
		h.mu.Lock()
		_, ok := h.execs[parts[1]]
		h.mu.Unlock()
		if !ok {
			answer(w, 404, `{"message":"No such exec instance"}`)
			return
		}
		h.hijack(w, r)
	case len(parts) == 2 && parts[0] == "containers" && r.Method == "DELETE":
		h.remove(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "containers":
		h.container(w, r, parts[1], parts[2])
	default:
		io.Copy(io.Discard, r.Body)
		answer(w, 404, `{"message":"page not found"}`)
	}
}

// labelFilter is the label filter of a list's filters, in either of the forms clients send.
func labelFilter(filters string) []string {
	if filters == "" {
		return nil
	}
	var asMap map[string]map[string]bool
	if json.Unmarshal([]byte(filters), &asMap) == nil {
		var out []string
		for l := range asMap["label"] {
			out = append(out, l)
		}
		return out
	}
	var asList map[string][]string
	json.Unmarshal([]byte(filters), &asList)
	return asList["label"]
}

// hasLabels is whether the container carries each of the filter's labels, a key or key=value.
func (c *hcontainer) hasLabels(want []string) bool {
	for _, l := range want {
		k, v, withValue := strings.Cut(l, "=")
		got, ok := c.labels[k]
		if !ok || withValue && got != v {
			return false
		}
	}
	return true
}

func (h *httpEngine) listContainers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	all := q.Get("all") == "1" || q.Get("all") == "true"
	labels := labelFilter(q.Get("filters"))
	h.mu.Lock()
	defer h.mu.Unlock()
	items := []string{}
	for _, c := range h.list {
		if !all && !c.running || !c.hasLabels(labels) {
			continue
		}
		lb, _ := json.Marshal(c.labels)
		state := "exited"
		if c.running {
			state = "running"
		}
		item := fmt.Sprintf(`{"Id":%s,"Names":[%s],"Image":%s,"Labels":%s,"State":%q`, jsonString(c.ID), jsonString("/"+c.Name), jsonString(c.Image), lb, state)
		if len(items) == 0 && h.pad > 0 {
			item += `,"Pad":"` + strings.Repeat("x", h.pad) + `"`
		}
		items = append(items, item+"}")
	}
	answer(w, 200, "["+strings.Join(items, ",")+"]")
}

func (h *httpEngine) create(w http.ResponseWriter, r *http.Request) {
	var spec struct {
		Image  string
		Labels map[string]string
	}
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil || spec.Image == "" {
		answer(w, 400, `{"message":"a create needs a JSON body with an Image"}`)
		return
	}
	if h.missing {
		answer(w, 404, fmt.Sprintf(`{"message":"No such image: %s"}`, spec.Image))
		return
	}
	name := strings.TrimPrefix(r.URL.Query().Get("name"), "/")
	h.mu.Lock()
	defer h.mu.Unlock()
	if name != "" && slices.ContainsFunc(h.list, func(c *hcontainer) bool { return c.Name == name }) {
		answer(w, 409, fmt.Sprintf(`{"message":"Conflict. The container name %q is already in use"}`, "/"+name))
		return
	}
	id := h.newID()
	if name == "" {
		name = "c" + id[:8]
	}
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	h.list = append(h.list, &hcontainer{Container: Container{ID: id, Name: name, Image: spec.Image, LimitBytes: uint64(h.memTotal)}, labels: spec.Labels})
	answer(w, 201, fmt.Sprintf(`{"Id":%q,"Warnings":[]}`, id))
}

// container answers the routes /containers/{ref}/{op}.
func (h *httpEngine) container(w http.ResponseWriter, r *http.Request, ref, op string) {
	h.mu.Lock()
	c := h.find(ref)
	h.mu.Unlock()
	if c == nil {
		io.Copy(io.Discard, r.Body)
		answer(w, 404, fmt.Sprintf(`{"message":"No such container: %s"}`, ref))
		return
	}
	set := func(running bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c.running == running && op != "restart" {
			w.WriteHeader(304)
			return
		}
		c.running = running
		w.WriteHeader(204)
	}
	switch r.Method + " " + op {
	case "POST start":
		set(true)
	case "POST restart":
		set(true)
	case "POST stop":
		set(false)
	case "POST wait":
		select {
		case <-time.After(h.slowWait):
		case <-r.Context().Done():
			return
		}
		h.mu.Lock()
		c.running = false // it ran to its end
		h.mu.Unlock()
		answer(w, 200, `{"StatusCode":0}`)
	case "POST attach":
		h.hijack(w, r)
	case "POST exec":
		io.Copy(io.Discard, r.Body)
		h.mu.Lock()
		defer h.mu.Unlock()
		if !c.running {
			answer(w, 409, fmt.Sprintf(`{"message":"container %s is not running"}`, c.ID))
			return
		}
		id := h.newID()
		h.execs[id] = c
		answer(w, 201, fmt.Sprintf(`{"Id":%q}`, id))
	case "GET json":
		h.mu.Lock()
		lb, _ := json.Marshal(c.labels)
		body := fmt.Sprintf(`{"Id":%q,"Name":%s,"Image":%s,"Config":{"Image":%s,"Labels":%s},"State":{"Running":%t}}`,
			c.ID, jsonString("/"+c.Name), jsonString(c.Image), jsonString(c.Image), lb, c.running)
		h.mu.Unlock()
		answer(w, 200, body)
	case "GET stats":
		h.stats(w, c)
	case "GET logs":
		h.logs(w, r, c)
	case "PUT archive":
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		h.mu.Lock()
		h.archives[c.ID+"\x00"+r.URL.Query().Get("path")] = b
		h.mu.Unlock()
		w.WriteHeader(200)
	case "GET archive":
		h.mu.Lock()
		b, ok := h.archives[c.ID+"\x00"+r.URL.Query().Get("path")]
		h.mu.Unlock()
		if !ok {
			answer(w, 404, `{"message":"Could not find the file in container"}`)
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Write(b)
	default:
		io.Copy(io.Discard, r.Body)
		answer(w, 404, `{"message":"page not found"}`)
	}
}

// stats answers a container's stats in the engine's shape: a stopped container's figures are zeros.
func (h *httpEngine) stats(w http.ResponseWriter, c *hcontainer) {
	h.mu.Lock()
	code, stopped, x := h.statsCode[c.ID], h.stopped[c.ID] || !c.running, c.Container
	h.mu.Unlock()
	switch {
	case code != 0:
		answer(w, code, `{"message":"status set by the test"}`)
	case stopped:
		answer(w, 200, fmt.Sprintf(`{"id":%q,"name":%s,"read":"0001-01-01T00:00:00Z","memory_stats":{}}`, x.ID, jsonString("/"+x.Name)))
	default:
		answer(w, 200, fmt.Sprintf(`{"id":%q,"name":%s,"read":"2026-01-01T00:00:00Z","memory_stats":{"usage":%d,"limit":%d,"stats":{"inactive_file":%d}}}`,
			x.ID, jsonString("/"+x.Name), x.UsageBytes, x.LimitBytes, x.InactiveFileBytes))
	}
}

// logs answers a container's logs as multiplexed frames: one line on each stream asked for.
func (h *httpEngine) logs(w http.ResponseWriter, r *http.Request, c *hcontainer) {
	q := r.URL.Query()
	stdout, stderr := q.Get("stdout") == "1" || q.Get("stdout") == "true", q.Get("stderr") == "1" || q.Get("stderr") == "true"
	if !stdout && !stderr {
		answer(w, 400, `{"message":"you must choose at least one stream"}`)
		return
	}
	w.Header().Set("Content-Type", MultiplexedStream)
	frame := func(stream byte, line string) {
		head := []byte{stream, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(head[4:], uint32(len(line)))
		w.Write(append(head, line...))
	}
	if stdout {
		frame(1, "stdout of "+c.ID[:12]+"\n")
	}
	if stderr {
		frame(2, "stderr of "+c.ID[:12]+"\n")
	}
}

func (h *httpEngine) remove(w http.ResponseWriter, r *http.Request, ref string) {
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.find(ref)
	switch {
	case c == nil:
		answer(w, 404, fmt.Sprintf(`{"message":"No such container: %s"}`, ref))
	case c.running && !force:
		answer(w, 409, `{"message":"cannot remove a running container"}`)
	default:
		h.list = slices.DeleteFunc(h.list, func(x *hcontainer) bool { return x == c })
		w.WriteHeader(204)
	}
}

// events streams a line at once and then one a second until the client or the test ends it.
func (h *httpEngine) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	rc := http.NewResponseController(w)
	for i := 1; ; i++ {
		fmt.Fprintf(w, `{"Type":"container","Action":"tick","seq":%d}`+"\n", i)
		if rc.Flush() != nil {
			return
		}
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		}
	}
}

// hijack takes the connection over as the engine does for an attach or exec start, and echoes what
// the client writes until it half-closes: raw after a 101 when the request asks for the upgrade,
// in chunks after a 200 of the raw-stream type when it does not.
func (h *httpEngine) hijack(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	upgrade := r.Header.Get("Upgrade") != ""
	c, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	h.mu.Lock()
	h.hijacked = append(h.hijacked, c)
	h.mu.Unlock()
	defer c.Close()
	if upgrade {
		rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: " + MultiplexedStream + "\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		if rw.Flush() != nil {
			return
		}
		io.Copy(c, rw.Reader)
		if u, ok := c.(*net.UnixConn); ok {
			u.CloseWrite()
		}
		return
	}
	rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: " + RawStream + "\r\nTransfer-Encoding: chunked\r\n\r\n")
	if rw.Flush() != nil {
		return
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := rw.Read(buf)
		if n > 0 {
			fmt.Fprintf(rw, "%x\r\n", n)
			rw.Write(buf[:n])
			rw.WriteString("\r\n")
			if rw.Flush() != nil {
				return
			}
		}
		if err != nil {
			break
		}
	}
	rw.WriteString("0\r\n\r\n")
	rw.Flush()
}
