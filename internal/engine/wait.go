//go:build unix

package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/state"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// StepResult is what one Step reports: Waiting or Admitted.
type StepResult interface{ stepResult() }

// Waiting reports a ticket still in the queue. Position is 1 when the last election made it the
// head, otherwise 1 plus the number of live, non-STALLED tickets ahead of it. Holders are the
// live holds at the last election.
type Waiting struct {
	Position int
	Holders  []HolderInfo
}

// Admitted carries the grant of an admitted ticket.
type Admitted struct {
	*Grant
}

func (Waiting) stepResult()  {}
func (Admitted) stepResult() {}

type mode int

const (
	modeQueued    mode = iota // a published ticket waits in the queue
	modeTokenOnly             // the format is not understood: no ticket, token-only admission
)

type status int

const (
	statusWaiting status = iota
	statusAdmitted
	statusCancelled // a cancel request was found
	statusWithdrawn // the owner withdrew
)

// aheadEntry is a well-formed ticket ahead of this one, from the last rescan.
type aheadEntry struct {
	name string
	key  state.Key
	dead bool
}

// holdEntry is a well-formed hold from the last rescan.
type holdEntry struct {
	name string
	info HolderInfo
}

// Ticket is one request waiting in the queue (or, in token-only mode, waiting for tokens). Its
// methods are safe for concurrent use: Steps are serialized, and Withdraw never waits for one.
type Ticket struct {
	e   *Engine
	req spec.Request

	stepMu sync.Mutex // serializes Step and Poll

	mu     sync.Mutex // guards status, grant and f, which Withdraw changes concurrently
	status status
	grant  *Grant
	f      *os.File // the ticket's lock-carrying descriptor

	// id and name are written holding both stepMu and mu (or before the ticket is shared).
	id   wire.ID
	name string

	// Everything below is guarded by stepMu.
	mode mode
	key  state.Key

	lastRefreshMono time.Duration
	lastRefreshReal int64
	needBump        bool // a refresh gap above StallAfter: bump gen in the section

	obs        state.Observer
	gen        store.Gen
	haveGen    bool
	ahead      []aheadEntry
	ticketOK   map[string]bool // ticket name -> body well-formed (bodies never change)
	holds      []holdEntry
	holdBodies map[string]*HolderInfo // hold name -> parsed body, nil when malformed

	nextElection time.Duration
	backoff      time.Duration
	attempts     int // admission attempts made, for tests
	last         Waiting
}

func newTicket(e *Engine, req spec.Request) *Ticket {
	return &Ticket{e: e, req: req, ticketOK: map[string]bool{}, holdBodies: map[string]*HolderInfo{},
		last: Waiting{Position: 1}}
}

// ID is the ticket's id, what a cancel request names; the zero ID in token-only mode.
func (t *Ticket) ID() wire.ID {
	_, id := t.ownName()
	return id
}

// Name is the ticket's file name in queue/; empty in token-only mode.
func (t *Ticket) Name() string {
	name, _ := t.ownName()
	return name
}

// TokenOnly reports whether the ticket waits in token-only mode.
func (t *Ticket) TokenOnly() bool {
	t.stepMu.Lock()
	defer t.stepMu.Unlock()
	return t.mode == modeTokenOnly
}

// Request is the normalized request.
func (t *Ticket) Request() spec.Request { return t.req }

// Step is one iteration of waiting (PROTOCOL.md section 10): refresh the ticket when due (and
// bump gen in the section after a refresh gap above 10 s on either clock), check cancel/ for
// this ticket, and, once per second, read gen (rescanning queue/ when it changed), observe the
// tickets ahead and hold the election; the head tries admission with backoff from 0.25 s up to
// 2 s, repeating the election before each attempt. It returns Waiting or Admitted, or
// ErrCancelled once a cancel request was found or the ticket was withdrawn. Section entries
// wait at most one second, so a caller that calls Step in a loop keeps refreshing. ctx bounds
// the section entries.
func (t *Ticket) Step(ctx context.Context) (StepResult, error) {
	return t.step(ctx, false)
}

// Poll is a Step whose section entries make a single attempt each (PROTOCOL.md section 8).
func (t *Ticket) Poll() (StepResult, error) {
	return t.step(context.Background(), true)
}

func (t *Ticket) step(ctx context.Context, single bool) (StepResult, error) {
	t.stepMu.Lock()
	defer t.stepMu.Unlock()
	if r, err, done := t.settled(); done {
		return r, err
	}
	now := t.e.clock.Monotonic()

	if now-t.lastRefreshMono >= RefreshEvery/2 {
		if err := t.refresh(now); err != nil {
			return t.settledOr(err)
		}
		if t.needBump {
			if err := t.resumeBump(); err != nil {
				return t.settledOr(err)
			}
		}
	}
	cancelled, err := t.cancelRequested()
	if err != nil {
		return t.settledOr(err)
	}
	if cancelled {
		return nil, t.onCancelled()
	}
	if now < t.nextElection {
		return t.last, nil
	}
	head, err := t.elect(now)
	if err != nil {
		return t.settledOr(err)
	}
	if !head {
		t.backoff = 0
		t.nextElection = now + ElectEvery
		return t.last, nil
	}
	t.attempts++
	g, err := t.admit(ctx, single)
	if err != nil {
		return t.settledOr(err)
	}
	if g != nil {
		return Admitted{g}, nil
	}
	if t.backoff == 0 {
		t.backoff = HeadFirstBackoff
	} else {
		t.backoff = min(2*t.backoff, HeadMaxBackoff)
	}
	t.nextElection = now + t.backoff
	return t.last, nil
}

// settled reports the outcome of a ticket that is no longer waiting.
func (t *Ticket) settled() (StepResult, error, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.status {
	case statusAdmitted:
		return Admitted{t.grant}, nil, true
	case statusCancelled, statusWithdrawn:
		return nil, ErrCancelled, true
	}
	return nil, nil, false
}

// settledOr returns the settled outcome when a concurrent Withdraw explains err (an operation
// on the closed descriptor), otherwise err.
func (t *Ticket) settledOr(err error) (StepResult, error) {
	if r, serr, done := t.settled(); done {
		return r, serr
	}
	return nil, err
}

// refresh sets the ticket's mtime through its descriptor (PROTOCOL.md section 10.2) and notes
// a gap above StallAfter on either clock since the previous refresh.
func (t *Ticket) refresh(now time.Duration) error {
	real := t.e.clock.Realtime()
	t.mu.Lock()
	f := t.f
	t.mu.Unlock()
	if err := touch(f, real); err != nil {
		return err
	}
	if now-t.lastRefreshMono > state.StallAfter || time.Duration(real-t.lastRefreshReal) > state.StallAfter {
		t.needBump = true
	}
	t.lastRefreshMono, t.lastRefreshReal = now, real
	return nil
}

// resumeBump bumps gen in the section after a long refresh gap, so that waiters rescan and see
// this ticket again. It never delays a refresh: it makes a single attempt to enter, and a busy
// section is retried at the next refresh (PROTOCOL.md section 10.2).
func (t *Ticket) resumeBump() error {
	sec, entered, err := t.e.st.TryEnter()
	if err != nil || !entered {
		return err
	}
	defer sec.Leave()
	n, understood, err := sec.Format(formats())
	if err != nil {
		return err
	}
	if !understood {
		return t.onUnknownFormat(n)
	}
	if _, err := sec.ReconcileIfDue(t.e.clock.Realtime()); err != nil {
		return err
	}
	if err := sec.BumpGen(); err != nil {
		return err
	}
	t.needBump = false
	return nil
}

// onUnknownFormat handles a FORMAT this client does not implement, found while waiting: no
// change is written, and the caller gets an error matching ErrRefused (and withdraws).
func (t *Ticket) onUnknownFormat(n int) error {
	return fmt.Errorf("%w: the state directory has format %d, which this client does not implement", ErrRefused, n)
}

// elect is the election (PROTOCOL.md section 10.3): it reads gen, rescans queue/ and holds/
// when gen changed, observes every ticket ahead (mtime and lock probe) on the monotonic clock,
// and reports whether no live, non-STALLED ticket is ahead. It updates t.last.
func (t *Ticket) elect(now time.Duration) (head bool, err error) {
	g, err := t.e.st.ReadGen()
	if err != nil {
		return false, err
	}
	if !t.haveGen || g.N != t.gen.N {
		if err := t.rescan(); err != nil {
			return false, err
		}
		t.gen, t.haveGen = g, true
	}
	tickets := make([]state.Ticket, 0, len(t.ahead))
	kept := t.ahead[:0]
	for _, a := range t.ahead {
		p := filepath.Join(t.e.st.Dir(), store.QueueDir, a.name)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			t.obs.Forget(a.key)
			continue
		}
		if err != nil {
			return false, err
		}
		dead, err := t.e.st.Probe(store.QueueDir, a.name)
		if errors.Is(err, fs.ErrNotExist) {
			t.obs.Forget(a.key)
			continue
		}
		if err != nil {
			return false, err
		}
		t.obs.Observe(a.key, fi.ModTime().UnixNano(), now)
		a.dead = dead
		kept = append(kept, a)
		tickets = append(tickets, state.Ticket{Key: a.key, Dead: dead})
	}
	t.ahead = kept
	head = state.Ahead(tickets, t.key, &t.obs, now)

	position := 1
	if !head {
		for _, a := range t.ahead {
			if !a.dead && !t.obs.Stalled(a.key, now) {
				position++
			}
		}
	}
	holders, err := t.liveHolders()
	if err != nil {
		return false, err
	}
	t.last = Waiting{Position: position, Holders: holders}
	return head, nil
}

// rescan lists queue/ and holds/. Tickets ahead of this one whose bodies are well-formed are
// kept for observation; malformed ones take no place in the queue (PROTOCOL.md section 7.2).
// Bodies never change once published, so each is read once.
func (t *Ticket) rescan() error {
	entries, _, err := t.e.st.List(store.QueueDir)
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(entries))
	var ahead []aheadEntry
	for _, en := range entries {
		present[en.Name] = true
		k := state.Key{OrderKey: en.ID.OrderKey, PID: en.ID.PID, Seq: en.ID.Seq}
		if !k.Less(t.key) {
			continue
		}
		ok, known := t.ticketOK[en.Name]
		if !known {
			body, err := t.e.readBody(store.QueueDir, en.Name)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			_, derr := wire.DecodeTicket(body)
			ok = derr == nil
			t.ticketOK[en.Name] = ok
		}
		if ok {
			ahead = append(ahead, aheadEntry{name: en.Name, key: k})
		}
	}
	for name := range t.ticketOK {
		if !present[name] {
			delete(t.ticketOK, name)
		}
	}
	for _, a := range t.ahead {
		if !present[a.name] {
			t.obs.Forget(a.key)
		}
	}
	t.ahead = ahead

	holds, _, err := t.e.st.List(store.HoldsDir)
	if err != nil {
		return err
	}
	presentHold := make(map[string]bool, len(holds))
	t.holds = t.holds[:0]
	for _, en := range holds {
		presentHold[en.Name] = true
		info, known := t.holdBodies[en.Name]
		if !known {
			body, err := t.e.readBody(store.HoldsDir, en.Name)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			info = holderInfo(en, body)
			t.holdBodies[en.Name] = info
		}
		if info != nil {
			t.holds = append(t.holds, holdEntry{name: en.Name, info: *info})
		}
	}
	for name := range t.holdBodies {
		if !presentHold[name] {
			delete(t.holdBodies, name)
		}
	}
	return nil
}

// holderInfo decodes a hold body; nil when it is malformed, including a start_ns that is not
// the name's start-ns.
func holderInfo(en store.Entry, body []byte) *HolderInfo {
	h, err := wire.DecodeHold(body)
	if err != nil || h.StartNS != en.ID.OrderKey {
		// A start_ns that differs from the name's makes the body malformed (PROTOCOL.md 7.4).
		return nil
	}
	grant := make(map[string]int, len(h.Grant))
	for name, idx := range h.Grant {
		grant[name] = len(idx)
	}
	return &HolderInfo{Name: en.Name, Label: h.Label, PID: en.ID.PID, StartNS: int64(en.ID.OrderKey),
		Grant: grant, Unbudgeted: h.Unbudgeted, UnbudgetedResources: h.UnbudgetedResources,
		Clamped: h.Clamped, Adopted: h.Adopted}
}

// liveHolders probes the holds of the last rescan and returns the live ones.
func (t *Ticket) liveHolders() ([]HolderInfo, error) {
	var live []HolderInfo
	for _, h := range t.holds {
		dead, err := t.e.st.Probe(store.HoldsDir, h.name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !dead {
			live = append(live, h.info)
		}
	}
	return live, nil
}
