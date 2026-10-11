//go:build unix

// Package engine runs the protocol's transitions (PROTOCOL.md sections 10 to 15): enqueue,
// observed-freshness waiting and election, cancellation and withdrawal, admission with clamping,
// adoption, token-only mode and inspection. It composes the pure state machine (internal/state)
// with the filesystem layer (internal/store). It never prints, never installs signal handlers,
// never chooses exit codes and never decides to run unbudgeted: errors go to the caller.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/state"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/testhooks"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// Timing of a waiter (PROTOCOL.md sections 10 and 22).
const (
	// RefreshEvery is the longest a waiter goes without refreshing its ticket. A Step refreshes
	// once half of it has passed, so a caller polling every RefreshEvery still refreshes in time.
	RefreshEvery = 2 * time.Second
	// ElectEvery is how often a waiter reads gen and repeats the election.
	ElectEvery = time.Second
	// Head admission retry backoff.
	HeadFirstBackoff = 250 * time.Millisecond
	HeadMaxBackoff   = 2 * time.Second
	// sectionWait bounds how long one Step waits to enter the section, so that a waiter keeps
	// refreshing while another client holds it.
	sectionWait = time.Second
	// releaseWait bounds how long a grant's Close waits to enter the section to remove its hold
	// (PROTOCOL.md section 12.3).
	releaseWait = 2 * time.Second
	// maxNameAttempts bounds the seq retries of a ticket whose final name exists.
	maxNameAttempts = 1000
)

// Clock is the engine's view of time. Realtime is the wall clock in nanoseconds since the
// epoch, including testhooks.RealtimeOffset; Monotonic is this process's own monotonic clock,
// which observed freshness is measured on and which is never written to disk.
type Clock struct {
	Realtime  func() int64
	Monotonic func() time.Duration
}

// SystemClock is the real clock: store.NowNS (which adds the test hook's offset) and the time
// since the call on Go's monotonic clock.
func SystemClock() Clock {
	start := time.Now()
	return Clock{
		Realtime:  store.NowNS,
		Monotonic: func() time.Duration { return time.Since(start) },
	}
}

// Seams the tests replace: the pause-point hook, the implemented formats and the seq counter.
var (
	point   = testhooks.Point
	formats = testhooks.Formats
)

// Engine runs requests against one opened state directory. It is safe for concurrent use.
type Engine struct {
	st    *store.Store
	cfg   *config.Watcher
	cpus  func() (int, error)
	clock Clock
	seq   func() int
}

// New returns an engine over st. cfg yields the configuration (under the state-directory
// override its Path is <statedir>/config.toml, which is then checked against the filesystem
// policy before every load); cpus yields the machine-wide online CPU count. A nil field of
// clock is replaced by SystemClock's.
func New(st *store.Store, cfg *config.Watcher, cpus func() (int, error), clock Clock) *Engine {
	sys := SystemClock()
	if clock.Realtime == nil {
		clock.Realtime = sys.Realtime
	}
	if clock.Monotonic == nil {
		clock.Monotonic = sys.Monotonic
	}
	return &Engine{st: st, cfg: cfg, cpus: cpus, clock: clock, seq: store.NextSeq}
}

// Store is the engine's state directory.
func (e *Engine) Store() *store.Store { return e.st }

// sizes recomputes every declared resource's size from the configuration and the CPU count
// (PROTOCOL.md section 5.2). An invalid configuration or an unknown CPU count is a
// *store.StateError.
func (e *Engine) sizes() (map[string]int, error) {
	if filepath.Dir(e.cfg.Path) == e.st.Dir() {
		// The override moved the configuration into the state directory: it is an entry of the
		// layout and must pass its policy (PROTOCOL.md section 5.3).
		if err := store.CheckEntry(e.cfg.Path, false); err != nil {
			return nil, err
		}
	}
	c, err := e.cfg.Current()
	if err != nil {
		return nil, &store.StateError{Reason: err.Error()}
	}
	n, err := e.cpus()
	if err != nil {
		return nil, &store.StateError{Reason: "machine-wide CPU count: " + err.Error()}
	}
	return c.Sizes(n), nil
}

// enter enters the section for one Step: a single TryEnter when single, otherwise Enter
// bounded by sectionWait. entered is false, with no error, when the section stayed busy; the
// caller's own ctx ending is an error.
func (e *Engine) enter(ctx context.Context, single bool) (sec *store.Section, entered bool, err error) {
	if single {
		return e.st.TryEnter()
	}
	wctx, cancel := context.WithTimeout(ctx, sectionWait)
	defer cancel()
	sec, err = e.st.Enter(wctx)
	if err == nil {
		return sec, true, nil
	}
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, false, nil
	}
	return nil, false, err
}

// Enqueue enters the section, reads FORMAT, reconciles if due, publishes a ticket for req
// (PROTOCOL.md section 10.1) keeping its lock-carrying descriptor, bumps gen and leaves. When
// the format is not one this client implements, nothing is written and the error matches
// ErrRefused. An invalid request
// is a *spec.UsageError. Enqueue waits for the section as long as it takes.
func (e *Engine) Enqueue(req spec.Request) (*Ticket, error) {
	req = spec.Normalize(req)
	if err := req.Validate(); err != nil {
		return nil, err
	}
	sec, err := e.st.Enter(context.Background())
	if err != nil {
		return nil, err
	}
	defer sec.Leave()
	n, understood, err := sec.Format(formats())
	if err != nil {
		return nil, err
	}
	if !understood {
		return nil, fmt.Errorf("%w: the state directory has format %d, which this client does not implement", ErrRefused, n)
	}
	t := newTicket(e, req)
	if err := t.publish(sec); err != nil {
		return nil, err
	}
	return t, nil
}

// publish reconciles if due and publishes t's ticket inside sec, then bumps gen. The ticket's
// name carries the order key of the wall clock read here; a name that exists takes the next
// seq.
func (t *Ticket) publish(sec *store.Section) error {
	e := t.e
	if _, err := sec.ReconcileIfDue(e.clock.Realtime()); err != nil {
		return err
	}
	arrival := e.clock.Realtime()
	body, err := wire.Encode(wire.Ticket{Label: t.req.Label, Priority: t.req.Priority,
		Need: t.req.Need, CreatedNS: arrival})
	if err != nil {
		return err
	}
	key := wire.OrderKey(arrival, t.req.Priority)
	pid := os.Getpid()
	for attempt := 0; attempt < maxNameAttempts; attempt++ {
		id := wire.ID{OrderKey: key, PID: pid, Seq: e.seq()}
		name := wire.TicketName(id, t.req.Label)
		f, err := sec.Publish(store.QueueDir, name, body, true)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := sec.BumpGen(); err != nil {
			f.Close() // the ticket is dead at once; reconciliation removes it
			return err
		}
		t.mu.Lock()
		t.f, t.id, t.name = f, id, name
		t.mu.Unlock()
		t.key = state.Key{OrderKey: id.OrderKey, PID: id.PID, Seq: id.Seq}
		t.lastRefreshMono, t.lastRefreshReal = e.clock.Monotonic(), arrival
		t.mode = modeQueued
		return nil
	}
	return fmt.Errorf("engine: no free ticket name after %d attempts", maxNameAttempts)
}

// readBody reads an entry of the layout of at most wire.MaxBodyBytes+1 bytes through the
// filesystem policy. A missing entry returns an error matching fs.ErrNotExist.
func (e *Engine) readBody(dir, name string) ([]byte, error) {
	f, err := store.OpenFile(filepath.Join(e.st.Dir(), dir, name), os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, wire.MaxBodyBytes+1))
}
