//go:build unix

package forward

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// MaxHead is the largest request or response head the tap reads; a larger one makes it blind.
const MaxHead = 64 << 10

// TapBuffer is the copy of each direction of a connection that the tap may lag behind the relay
// by. Measured on Docker Desktop with 50 parallel calls beside a 79 MiB image transfer, no tap fell
// behind at 32 KiB, and the transfer's did at 16 KiB; this is eight times the 32 KiB.
const TapBuffer = 256 << 10

// The kinds of an event line.
const (
	KindReady     = "ready"     // the entry is born, once
	KindConnected = "connected" // the first connection, once
	KindCreated   = "created"   // a create answered 201, with the id of its answer
	KindTouched   = "touched"   // a start, restart or exec answered 2xx, with the reference as written
	KindBlind     = "blind"     // the tap lost track: the run's figures are not to be trusted
)

// maxLine is the longest event line, its LF included.
const maxLine = 256

// queueLen is how many events wait for the run before the tap gives up on it.
const queueLen = 1024

// Event is one line of the pipe to the run.
type Event struct{ Kind, Ref string }

// Line is the event as written to the run: its kind, a space and its reference when it has one,
// and LF. A reference that would make the line longer than 256 bytes or break it into another
// line or field makes the line blind.
func (e Event) Line() string {
	if e.Ref == "" {
		return e.Kind + "\n"
	}
	l := e.Kind + " " + e.Ref + "\n"
	if len(l) > maxLine || strings.ContainsFunc(e.Ref, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return KindBlind + "\n"
	}
	return l
}

// ParseEvent reads a line as Line writes it, with or without its LF: a known kind, a reference
// exactly for created and touched, and at most 256 bytes.
func ParseEvent(line string) (Event, bool) {
	if len(strings.TrimSuffix(line, "\n"))+1 > maxLine {
		return Event{}, false
	}
	kind, ref, spaced := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
	switch kind {
	case KindCreated, KindTouched:
		if ref == "" || strings.ContainsAny(ref, " \n") {
			return Event{}, false
		}
	case KindReady, KindConnected, KindBlind:
		if spaced {
			return Event{}, false
		}
	default:
		return Event{}, false
	}
	return Event{kind, ref}, true
}

// Tap writes the events of a forwarder's connections to the run, one line each, through a bounded
// queue, so that the relay never waits on the run: a full queue writes blind once and drops the
// rest.
type Tap struct {
	w         io.Writer
	q         chan Event
	full      atomic.Bool
	overflow  chan struct{} // closed when the queue was full
	connected atomic.Bool
	written   chan struct{} // closed when the writer is done
}

// NewTap starts the writer of events to w.
func NewTap(w io.Writer) *Tap {
	t := &Tap{w: w, q: make(chan Event, queueLen), overflow: make(chan struct{}), written: make(chan struct{})}
	go t.write()
	return t
}

// Send queues an event, or gives up on the run when the queue is full.
func (t *Tap) Send(e Event) {
	if t.full.Load() {
		return
	}
	select {
	case t.q <- e:
	default:
		if !t.full.Swap(true) {
			close(t.overflow)
		}
	}
}

// Close writes what is queued and returns; nothing may be sent after it.
func (t *Tap) Close() {
	close(t.q)
	<-t.written
}

// write writes each event until the queue closes; after a failed write it only drains.
func (t *Tap) write() {
	defer close(t.written)
	broken := false
	put := func(e Event) {
		if !broken {
			_, err := io.WriteString(t.w, e.Line())
			broken = err != nil
		}
	}
	for {
		select {
		case e, ok := <-t.q:
			if !ok {
				return
			}
			put(e)
		case <-t.overflow:
			put(Event{Kind: KindBlind})
			for range t.q {
			}
			return
		}
	}
}

// The states of a connection's tap.
const (
	tapping int32 = iota
	ended         // at an upgrade: the rest is a raw stream
	blinded
)

// parseHold runs at the start of each parser; a test stalls the parsers with it.
var parseHold = func() {}

// connTap follows one connection: each direction's bytes are copied into a bounded buffer, and two
// parsers read the requests and the answers from them and pair them in order.
type connTap struct {
	tap      *Tap
	state    atomic.Int32
	req      *bounded
	resp     *bounded
	pending  chan *pending
	respDone chan struct{} // closed when the answers' parser returns
	stopped  chan struct{} // closed when the tap stops tapping
	stop     sync.Once
	parsers  sync.WaitGroup
}

// pending is a request waiting for its answer.
type pending struct {
	method     string
	create     bool
	touch      string // the reference of a start, restart or exec
	hijackable bool   // an attach, an exec start or an upgrade, which may take the connection over
	verdict    chan bool
}

// conn starts the tap of a new connection; the forwarder's first says connected.
func (t *Tap) conn() *connTap {
	if !t.connected.Swap(true) {
		t.Send(Event{Kind: KindConnected})
	}
	c := &connTap{tap: t, req: newBounded(), resp: newBounded(), pending: make(chan *pending, 16), respDone: make(chan struct{}), stopped: make(chan struct{})}
	c.parsers.Add(2)
	go c.requests()
	go c.responses()
	return c
}

// feed copies bytes the relay passed in one direction; a copy that does not fit makes the tap blind.
func (c *connTap) feed(b *bounded, p []byte) {
	if c.state.Load() == tapping && !b.offer(p) {
		c.finish(blinded)
	}
}

// close marks the end of a direction's bytes.
func (c *connTap) close(b *bounded) { b.close() }

// wait returns when both parsers have.
func (c *connTap) wait() { c.parsers.Wait() }

// finish stops the tap, normally at an upgrade or blind, once.
func (c *connTap) finish(to int32) {
	if !c.state.CompareAndSwap(tapping, to) {
		return
	}
	if to == blinded {
		c.tap.Send(Event{Kind: KindBlind})
	}
	c.stop.Do(func() {
		close(c.stopped)
		c.req.abort()
		c.resp.abort()
	})
}

// fail ends the tap on a parser's error: blind, unless the bytes ran out because the connection
// ended or the tap had already stopped.
func (c *connTap) fail(b *bounded, err error) {
	if errors.Is(err, errAborted) || !errors.Is(err, errOversized) && b.ran() && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		return
	}
	c.finish(blinded)
}

// versionPrefix is the API version a client may put before a route.
var versionPrefix = regexp.MustCompile(`^/v[0-9]+(\.[0-9]+)?/`)

// classify is what the tap needs of a request.
func classify(r *http.Request) *pending {
	p := &pending{method: r.Method, verdict: make(chan bool, 1), hijackable: r.Header.Get("Upgrade") != ""}
	if r.Method != "POST" {
		return p
	}
	path := versionPrefix.ReplaceAllString(r.URL.EscapedPath(), "/")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case path == "/containers/create":
		p.create = true
	case len(parts) == 3 && parts[0] == "containers" && (parts[2] == "start" || parts[2] == "restart" || parts[2] == "exec"):
		p.touch = parts[1]
	case len(parts) == 3 && (parts[0] == "containers" && parts[2] == "attach" || parts[0] == "exec" && parts[2] == "start"):
		p.hijackable = true
	}
	return p
}

// requests reads the requests in order, hands each to the answers' parser, and after one that may
// take the connection over waits for its answer before reading on.
func (c *connTap) requests() {
	defer c.parsers.Done()
	defer close(c.pending)
	parseHold()
	m := newMessages(c.req)
	for m.more() {
		var r *http.Request
		err := m.head(func(br *bufio.Reader) (err error) { r, err = http.ReadRequest(br); return err })
		if err != nil {
			c.fail(c.req, err)
			return
		}
		p := classify(r)
		select {
		case c.pending <- p:
		case <-c.stopped:
			return
		case <-c.respDone:
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			c.fail(c.req, err)
			return
		}
		if p.hijackable {
			select {
			case over := <-p.verdict:
				if over {
					return
				}
			case <-c.stopped:
				return
			case <-c.respDone:
				return
			}
		}
	}
}

// isStream is whether a content type is one of an answer that takes the connection over.
func isStream(contentType string) bool {
	mt, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(mt)) {
	case "application/vnd.docker.raw-stream", "application/vnd.docker.multiplexed-stream":
		return true
	}
	return false
}

// responses reads the answers in order, pairs each with its request, interim answers skipped, and
// reports what a create or a touch's answer says; it stops the tap at an answer that takes the
// connection over.
func (c *connTap) responses() {
	defer c.parsers.Done()
	defer close(c.respDone)
	parseHold()
	m := newMessages(c.resp)
	for m.more() {
		var p *pending
		select {
		case q, ok := <-c.pending:
			if !ok {
				c.finish(blinded) // an answer to no request
				return
			}
			p = q
		case <-c.stopped:
			return
		}
		var r *http.Response
		for {
			err := m.head(func(br *bufio.Reader) (err error) {
				r, err = http.ReadResponse(br, &http.Request{Method: p.method})
				return err
			})
			if err != nil {
				c.fail(c.resp, err)
				return
			}
			if r.StatusCode == http.StatusSwitchingProtocols || r.StatusCode >= 200 {
				break
			}
		}
		over := r.StatusCode == http.StatusSwitchingProtocols || p.hijackable && r.StatusCode == 200 && isStream(r.Header.Get("Content-Type"))
		if over {
			p.verdict <- true
			c.finish(ended)
			return
		}
		ok := r.StatusCode >= 200 && r.StatusCode < 300
		switch {
		case p.create && r.StatusCode == http.StatusCreated:
			var answer struct{ Id string }
			if json.NewDecoder(io.LimitReader(r.Body, MaxHead)).Decode(&answer) != nil || answer.Id == "" {
				c.finish(blinded)
				return
			}
			c.tap.Send(Event{KindCreated, answer.Id})
		case p.touch != "" && ok:
			c.tap.Send(Event{KindTouched, p.touch})
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			c.fail(c.resp, err)
			return
		}
		p.verdict <- false
	}
}

// errOversized is a head over MaxHead.
var errOversized = errors.New("forward: head over the limit")

// messages reads one direction's messages, counting each head's bytes.
type messages struct {
	lr    io.LimitedReader
	count int64 // bytes read from lr
	br    *bufio.Reader
}

func newMessages(b *bounded) *messages {
	m := &messages{lr: io.LimitedReader{R: b, N: math.MaxInt64}}
	m.br = bufio.NewReader(readCounter{m})
	return m
}

type readCounter struct{ m *messages }

func (r readCounter) Read(p []byte) (int, error) {
	n, err := r.m.lr.Read(p)
	r.m.count += int64(n)
	return n, err
}

// pos is how many bytes the messages read so far have taken.
func (m *messages) pos() int64 { return m.count - int64(m.br.Buffered()) }

// more is whether another message begins: false at the end of the bytes.
func (m *messages) more() bool {
	_, err := m.br.Peek(1)
	return err == nil
}

// head reads one head with read, at most MaxHead bytes of it: the reads below it stop a little
// past the limit, and the count of the bytes it took decides.
func (m *messages) head(read func(*bufio.Reader) error) error {
	start := m.pos()
	m.lr.N = MaxHead + int64(m.br.Size())
	err := read(m.br)
	if err != nil && m.lr.N <= 0 || m.pos()-start > MaxHead {
		return errOversized
	}
	m.lr.N = math.MaxInt64
	return err
}

// errAborted is the read of a tap that stopped.
var errAborted = errors.New("forward: tap stopped")

// bounded is a buffer of at most TapBuffer bytes between the relay and a parser.
type bounded struct {
	mu      sync.Mutex
	cond    sync.Cond
	buf     []byte
	closed  bool // the direction ended
	aborted bool // the tap stopped
}

func newBounded() *bounded {
	b := &bounded{}
	b.cond.L = &b.mu
	return b
}

// offer copies p in, or reports that it does not fit.
func (b *bounded) offer(p []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.aborted {
		return true
	}
	if len(b.buf)+len(p) > TapBuffer {
		return false
	}
	b.buf = append(b.buf, p...)
	b.cond.Signal()
	return true
}

func (b *bounded) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.buf) == 0 && !b.closed && !b.aborted {
		b.cond.Wait()
	}
	switch {
	case b.aborted:
		return 0, errAborted
	case len(b.buf) == 0:
		return 0, io.EOF
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

// ran is whether the direction ended, so that an end of bytes is the connection's.
func (b *bounded) ran() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func (b *bounded) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.cond.Broadcast()
}

func (b *bounded) abort() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.aborted, b.buf = true, nil
	b.cond.Broadcast()
}
