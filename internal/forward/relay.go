//go:build unix

package forward

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Config is one forwarder's: Entry is its final directory in the forwarder's directory, Upstream
// the path of Docker's socket, End the read end of a pipe whose end of input is the end of the run,
// and Events the write end of the pipe to the run, whose first line is ready once the entry is born.
type Config struct {
	Entry    string
	Upstream string
	End      *os.File
	Events   *os.File
}

// drainGrace is how long the listener keeps accepting after the swap: a client that connected just
// before it is in the listener's queue, and is served.
const drainGrace = 100 * time.Millisecond

// copyBuffer is the size of each direction's reads.
const copyBuffer = 32 << 10

// Serve births the entry, says ready, and relays each connection to the upstream until End reads
// its end; then it swaps the address for a symlink to the upstream, serves only the connections
// already open, and returns when the last of them closes. A failed accept never ends it.
func Serve(cfg Config) error {
	lock, ln, err := Birth(cfg.Entry)
	if err != nil {
		return err
	}
	defer lock.Close()
	r := newRelay(ln, cfg.Upstream)
	if _, err := io.WriteString(cfg.Events, "ready\n"); err != nil {
		ln.Close()
		return err
	}
	go r.accept()
	io.Copy(io.Discard, cfg.End) // any end of End, an error included, is the end of the run
	err = Swap(cfg.Entry, cfg.Upstream)
	r.stop()
	return err
}

// relay copies each connection its listener accepts to a connection of its own to the upstream.
type relay struct {
	ln       net.Listener
	upstream string
	stopping atomic.Bool
	accepted chan struct{} // closed when accept returns
	conns    sync.WaitGroup
}

func newRelay(ln net.Listener, upstream string) *relay {
	return &relay{ln: ln, upstream: upstream, accepted: make(chan struct{})}
}

// accept serves every connection until stop; a failed accept is retried after a pause that
// doubles up to a second, so that running out of descriptors neither spins nor ends the forwarder.
func (r *relay) accept() {
	defer close(r.accepted)
	pause := 5 * time.Millisecond
	for {
		c, err := r.ln.Accept()
		if err != nil {
			if r.stopping.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(pause)
			pause = min(2*pause, time.Second)
			continue
		}
		pause = 5 * time.Millisecond
		r.conns.Add(1)
		go r.handle(c)
	}
}

// stop accepts what is already queued for drainGrace, closes the listener and waits for the open
// connections.
func (r *relay) stop() {
	r.stopping.Store(true)
	if d, ok := r.ln.(interface{ SetDeadline(time.Time) error }); ok {
		d.SetDeadline(time.Now().Add(drainGrace))
	} else {
		r.ln.Close()
	}
	<-r.accepted
	r.ln.Close()
	r.conns.Wait()
}

// handle relays one connection both ways; each direction's end of input half-closes the other side,
// and a failure in either direction closes both.
func (r *relay) handle(c net.Conn) {
	defer r.conns.Done()
	defer c.Close()
	up, err := net.DialTimeout("unix", r.upstream, 5*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	var both sync.WaitGroup
	both.Go(func() { pipe(up, c) })
	both.Go(func() { pipe(c, up) })
	both.Wait()
}

// pipe copies src to dst until src ends, then half-closes dst; on a failure it closes both, which
// ends the other direction too.
func pipe(dst, src net.Conn) {
	buf := make([]byte, copyBuffer)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				src.Close()
				dst.Close()
				return
			}
		}
		if errors.Is(err, io.EOF) {
			if hc, ok := dst.(interface{ CloseWrite() error }); ok {
				hc.CloseWrite()
			}
			return
		}
		if err != nil {
			src.Close()
			dst.Close()
			return
		}
	}
}
