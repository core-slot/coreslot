//go:build unix

// Package engine runs the protocol's transitions (PROTOCOL.md sections 10 to 15): enqueue,
// observed-freshness waiting and election, cancellation and withdrawal, admission with clamping,
// adoption, token-only mode and inspection, and the pass-through of nested requests (section 18).
// It composes the pure state machine (internal/state) with the filesystem layer
// (internal/store). It never prints, never installs signal handlers, never chooses exit codes and
// never decides on its own to run unbudgeted: errors go to the caller, and a pass-through under a
// parent that runs unbudgeted follows the parent.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/testhooks"
	"github.com/core-slot/coreslot/internal/wire"
)

// Timing of a waiter (PROTOCOL.md sections 10 and 22).
const (
	// RefreshEvery is the longest a waiter goes between two refreshes of its ticket (PROTOCOL.md
	// section 10.2). A Step refreshes when RefreshEvery/2 has passed since the last refresh, and
	// again just before a section entry that may wait once RefreshEvery/4 has passed; such an
	// entry waits at most sectionWait = RefreshEvery/4. Ticket.Due is never later than the next
	// RefreshEvery/2 after a refresh, so a caller that Steps when Due says (Wait and the CLI do)
	// refreshes at most RefreshEvery/2 apart, plus its own wake-up latency and the filesystem work
	// of one Step: a Step that starts just under RefreshEvery/4 after a refresh does not refresh
	// before its entry, the entry waits sectionWait and the next Step, due RefreshEvery/2 after
	// the refresh, refreshes. A caller that instead starts each Step within RefreshEvery/8 of the
	// previous one returning refreshes at most RefreshEvery/4 + RefreshEvery/4 + RefreshEvery/8
	// = 1.25 s apart. A Poll refreshes whenever RefreshEvery/2 has passed, so its refresh gaps
	// are its caller's poll interval, at most 2 s (PROTOCOL.md section 10.2).
	RefreshEvery = 2 * time.Second
	// ElectEvery is how often a waiter reads gen and repeats the election.
	ElectEvery = time.Second
	// Head admission retry backoff.
	HeadFirstBackoff = 250 * time.Millisecond
	HeadMaxBackoff   = 2 * time.Second
	// sectionWait bounds how long one Step waits to enter the section, so that a waiter keeps
	// refreshing while another client holds it (see RefreshEvery).
	sectionWait = RefreshEvery / 4
	// releaseWait bounds how long a grant's Close waits to enter the section to remove its hold
	// (PROTOCOL.md section 12.3).
	releaseWait = 2 * time.Second
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

// Seams the tests replace: the pause-point hook and the two ways of entering the section,
// through which every entry of the engine goes.
var (
	point           = testhooks.Point
	enterSection    = func(st *store.Store, ctx context.Context) (*store.Section, error) { return st.Enter(ctx) }
	tryEnterSection = func(st *store.Store) (*store.Section, bool, error) { return st.TryEnter() }
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

// Config reads the configuration as every admission does, afresh. An entry that fails the
// layout's policy or an invalid configuration is a *store.StateError.
func (e *Engine) Config() (config.Config, error) {
	if filepath.Dir(e.cfg.Path) == e.st.Dir() {
		// The override moved the configuration into the state directory: it is an entry of the
		// layout and must pass its policy (PROTOCOL.md section 5.3).
		if err := store.CheckEntry(e.cfg.Path, false); err != nil {
			return config.Config{}, err
		}
	}
	c, err := e.cfg.Current()
	if err != nil {
		return config.Config{}, &store.StateError{Reason: err.Error(), Err: err}
	}
	return c, nil
}

// sizes recomputes every declared resource's size from the configuration and the CPU count
// (PROTOCOL.md section 5.2). An invalid configuration or an unknown CPU count is a
// *store.StateError.
func (e *Engine) sizes() (map[string]int, error) {
	c, err := e.Config()
	if err != nil {
		return nil, err
	}
	n, err := e.cpus()
	if err != nil {
		return nil, &store.StateError{Reason: "machine-wide CPU count: " + err.Error(), Err: err}
	}
	return c.Sizes(n), nil
}

// refuseFormat is the refusal of a write into a state directory whose FORMAT names format n,
// which this client does not implement: it matches ErrRefused and is a *store.FormatError, and
// suffix ends its message.
func refuseFormat(n int, suffix string) error {
	return fmt.Errorf("%w: %w", ErrRefused, &store.FormatError{N: n, Suffix: suffix})
}

// enter enters the section for one Step: a single TryEnter when single, otherwise Enter
// bounded by sectionWait. entered is false, with no error, when the section stayed busy; the
// caller's own ctx ending is an error.
func (e *Engine) enter(ctx context.Context, single bool) (sec *store.Section, entered bool, err error) {
	if single {
		return tryEnterSection(e.st)
	}
	wctx, cancel := context.WithTimeout(ctx, sectionWait)
	defer cancel()
	sec, err = enterSection(e.st, wctx)
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

// enterUntil enters the section for an operation that waits as long as its caller lets it
// (enqueue, adoption, cancellation): Enter's backoff from 10 ms up to 250 ms until ctx ends
// (PROTOCOL.md section 8). A ctx that ends while another client holds the section is a
// *SectionBusyError naming the pid admit.lock records; one that had ended before is ctx.Err().
func (e *Engine) enterUntil(ctx context.Context) (*store.Section, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sec, err := enterSection(e.st, ctx)
	if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		busy := &SectionBusyError{Err: err}
		if a, ok := e.sectionHolder(); ok {
			busy.PID = a.PID
		}
		return nil, busy
	}
	return sec, err
}

// EnqueueContext enters the section, reads FORMAT, reconciles if due, publishes a ticket for
// req (PROTOCOL.md section 10.1) keeping its lock-carrying descriptor, bumps gen and leaves. When
// the format is not one this client implements, nothing is written and the returned ticket is
// in token-only mode (section 15.2): its Step tries token-only admission. An invalid request
// is a *spec.UsageError. Its wait for the section ends with ctx: it then returns a
// *SectionBusyError matching ctx.Err() (or ctx.Err() itself when ctx had already ended) having
// published nothing. Once inside the section it completes.
func (e *Engine) EnqueueContext(ctx context.Context, req spec.Request) (*Ticket, error) {
	req = spec.Normalize(req)
	if err := req.Validate(); err != nil {
		return nil, err
	}
	sec, err := e.enterUntil(ctx)
	if err != nil {
		return nil, err
	}
	defer sec.Leave()
	_, understood, err := sec.Format()
	if err != nil {
		return nil, err
	}
	t := newTicket(e, req)
	if !understood {
		t.mode = modeTokenOnly
		t.last = Waiting{}
		return t, nil
	}
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
	id := wire.ID{OrderKey: wire.OrderKey(arrival, t.req.Priority), PID: os.Getpid()}
	f, name, seq, err := sec.PublishNamed(store.QueueDir, body, e.seq(), e.seq, func(seq int) string {
		return wire.TicketName(wire.ID{OrderKey: id.OrderKey, PID: id.PID, Seq: seq}, t.req.Label)
	})
	if err != nil {
		return err
	}
	id.Seq = seq
	if err := sec.BumpGen(); err != nil {
		f.Close() // the ticket is dead at once; reconciliation removes it
		return err
	}
	t.mu.Lock()
	if t.status != statusWaiting {
		// Withdrawn while re-enqueueing from token-only mode: the new ticket is dead at once.
		t.mu.Unlock()
		f.Close()
		return ErrCanceled
	}
	t.f, t.id, t.name = f, id, name
	t.mu.Unlock()
	t.key = state.Key(id)
	t.lastRefreshMono, t.lastRefreshReal = e.clock.Monotonic(), arrival
	t.mode = modeQueued
	return nil
}

// readBody reads the entry dir/name of the layout, of at most wire.MaxBodyBytes+1 bytes, through
// the filesystem policy. A missing entry returns an error matching fs.ErrNotExist.
func (e *Engine) readBody(dir, name string) ([]byte, error) {
	return e.st.ReadSmall(filepath.Join(dir, name), wire.MaxBodyBytes)
}

// gone reports an entry that no longer exists (PROTOCOL.md section 10.3: a vanished entry is
// gone, never an error).
func gone(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// stray reports an entry of queue/, holds/ or cancel/ that fails the filesystem policy when
// opened or probed (a symlink, a directory, another user's or group/world-writable file put in
// place after the listing): it is ignored and counted, never trusted (PROTOCOL.md section 4.2).
func stray(err error) bool {
	var se *store.StateError
	return errors.As(err, &se)
}
