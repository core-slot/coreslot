//go:build unix

package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/state"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// StepResult is what one Step reports: Waiting or Admitted.
type StepResult interface{ stepResult() }

// Waiting reports a ticket still waiting. Position is 1 when the last election made it the
// head, otherwise 1 plus the number of tickets ahead of it that the election did not pass over:
// the election stops at the first live, non-STALLED ticket, so the tickets beyond it are
// counted unexamined and Position is an upper bound. It is 0 in token-only mode, which has no
// place in the queue. Holders is empty in a Step's result: Ticket.Holders lists the live holds
// when a caller needs them (the CLI's waiting line, every 30 s).
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
}

// Seams the tests replace to count a waiter's filesystem work: the election's mtime read and
// lock probe of a ticket ahead, the gen read and the cancel check.
var (
	lstat       = os.Lstat
	probe       = (*store.Store).Probe
	genRead     = (*store.Store).ReadGen
	checkCancel = (*Ticket).cancelRequested
)

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
	ahead      []aheadEntry           // ascending: the nearest ticket ahead is the last
	ticketOK   map[string]bool        // ticket name -> body well-formed (bodies never change)
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
// this ticket, and, once per second, read gen (rescanning queue/ when it changed) and hold the
// election, which examines the tickets ahead from the nearest up to the first live, non-STALLED
// one; the head tries admission with backoff from 0.25 s up to 2 s, repeating the election
// before each attempt. It returns Waiting or Admitted, or ErrCancelled once a cancel request
// was found or the ticket was withdrawn. Section entries wait at most RefreshEvery/4, so a
// caller that calls Step in a loop keeps refreshing at least every RefreshEvery (see
// RefreshEvery). A caller that calls Step again after Due has no more work per Step than the
// protocol's: a waiter that is not the head Steps once per second. ctx bounds the section
// entries.
func (t *Ticket) Step(ctx context.Context) (StepResult, error) {
	return t.step(ctx, false)
}

// Poll is a Step whose section entries make a single attempt each (PROTOCOL.md section 8).
// While another Step or Poll of this ticket runs, it returns at once: the settled outcome, or a
// zero Waiting.
func (t *Ticket) Poll() (StepResult, error) {
	return t.step(context.Background(), true)
}

// Due is how long from now until the ticket next has work due: the next election or head
// attempt (token-only: the next attempt) or the next refresh, whichever is first; 0 when it is
// due now or the ticket is settled. A caller that Steps after Due checks cancel/ on each of
// those Steps, so a waiter that is not the head does all of its work once per second: refresh,
// cancel check, gen read and election.
func (t *Ticket) Due() time.Duration {
	t.stepMu.Lock()
	defer t.stepMu.Unlock()
	if _, _, done := t.settled(); done {
		return 0
	}
	next := t.nextElection
	if t.mode == modeQueued {
		next = min(next, t.lastRefreshMono+RefreshEvery/2)
	}
	return max(0, next-t.e.clock.Monotonic())
}

func (t *Ticket) step(ctx context.Context, single bool) (StepResult, error) {
	if single {
		// A Poll never waits for a concurrent Step: that Step is doing the work.
		if !t.stepMu.TryLock() {
			if r, err, done := t.settled(); done {
				return r, err
			}
			return Waiting{}, nil
		}
	} else {
		t.stepMu.Lock()
	}
	defer t.stepMu.Unlock()
	if r, err, done := t.settled(); done {
		return r, err
	}
	now := t.e.clock.Monotonic()
	if t.mode == modeTokenOnly {
		return t.stepTokenOnly(ctx, single, now)
	}

	if now-t.lastRefreshMono >= RefreshEvery/2 {
		if err := t.refresh(now); err != nil {
			return t.settledOr(err)
		}
		if t.needBump {
			if err := t.resumeBump(); err != nil {
				return t.settledOr(err)
			}
			if t.mode == modeTokenOnly {
				return t.last, nil
			}
		}
	}
	cancelled, err := checkCancel(t)
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
	// Refresh again before an entry that may wait, so the entry cannot stretch the gap.
	if !single {
		if now := t.e.clock.Monotonic(); now-t.lastRefreshMono >= RefreshEvery/4 {
			if err := t.refresh(now); err != nil {
				return t.settledOr(err)
			}
		}
	}
	t.attempts++
	g, err := t.admit(ctx, single)
	if err != nil {
		return t.settledOr(err)
	}
	if g != nil {
		return Admitted{g}, nil
	}
	if t.mode == modeTokenOnly {
		return t.last, nil // the format changed under the ticket: token-only from the next Step
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
	point("ticket.refreshed")
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
	sec, entered, err := tryEnterSection(t.e.st)
	if err != nil || !entered {
		return err
	}
	defer sec.Leave()
	_, understood, err := sec.Format(formats())
	if err != nil {
		return err
	}
	if !understood {
		t.toTokenOnly()
		return nil
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

// elect is the election (PROTOCOL.md section 10.3): it reads gen, rescans queue/ when gen
// changed, and examines the tickets
// ahead from the nearest, observing each one's mtime on the monotonic clock and probing its
// lock: a gone, dead or STALLED one is passed over and the first live, non-STALLED one ends the
// examination. The tickets beyond it are not examined in this pass (state.Observer counts a stall
// only across consecutive examinations), except that the first FrontWindow tickets of the queue
// are always watched (watchFront). It reports whether no live, non-STALLED ticket
// is ahead and updates t.last.
func (t *Ticket) elect(now time.Duration) (head bool, err error) {
	g, err := genRead(t.e.st)
	if err != nil {
		return false, err
	}
	if !t.haveGen || g.N != t.gen.N {
		if err := t.rescan(); err != nil {
			return false, err
		}
		t.gen, t.haveGen = g, true
	}
	t.obs.Pass(now)
	head = true
	passed := 0 // tickets examined and passed over, still listed
	stop := 0   // the scan examined t.ahead[stop:]
	for i := len(t.ahead) - 1; i >= 0; i-- {
		stop = i
		a := t.ahead[i]
		fi, err := lstat(filepath.Join(t.e.st.Dir(), store.QueueDir, a.name))
		if gone(err) {
			t.obs.Forget(a.key)
			t.ahead = slices.Delete(t.ahead, i, i+1) // only nearer entries, already examined, shift
			continue
		}
		if err != nil {
			return false, err
		}
		dead, err := probe(t.e.st, store.QueueDir, a.name)
		if gone(err) || stray(err) {
			// Gone, or replaced by a stray entry: either way it takes no place in the queue.
			t.obs.Forget(a.key)
			t.ahead = slices.Delete(t.ahead, i, i+1)
			continue
		}
		if err != nil {
			return false, err
		}
		if !dead {
			t.obs.Observe(a.key, fi.ModTime().UnixNano(), now)
		}
		if state.PassedOver(state.Ticket{Key: a.key, Dead: dead}, &t.obs, now) {
			passed++
			continue
		}
		head = false
		break
	}
	if err := t.watchFront(stop, now); err != nil {
		return false, err
	}
	position := 1
	if !head {
		position += len(t.ahead) - passed
	}
	t.last = Waiting{Position: position}
	return head, nil
}

// FrontWindow is how many tickets at the front of the queue every waiter watches in each
// election besides those its scan examines (PROTOCOL.md section 10.3). Watching them keeps every
// waiter's timer on a stopped head running while a live ticket nearer to it ends its scan, so
// up to FrontWindow consecutive stopped tickets at the front stop blocking the whole queue
// within the I5 bound, not one successor after another.
const FrontWindow = 4

// watchFront observes the mtime of the first FrontWindow tickets ahead that are not gone,
// except those the scan examined in this pass (t.ahead[stop:]). It only observes: the election
// still decides from the scan alone.
func (t *Ticket) watchFront(stop int, now time.Duration) error {
	for j, watched := 0, 0; j < stop && watched < FrontWindow; {
		a := t.ahead[j]
		fi, err := lstat(filepath.Join(t.e.st.Dir(), store.QueueDir, a.name))
		if gone(err) {
			t.obs.Forget(a.key)
			t.ahead = slices.Delete(t.ahead, j, j+1)
			stop--
			continue
		}
		if err != nil {
			return err
		}
		t.obs.Observe(a.key, fi.ModTime().UnixNano(), now)
		watched++
		j++
	}
	return nil
}

// rescan re-lists the names in queue/ (the listing alone: no entry's metadata is read, so its
// cost is the directory read). Tickets ahead of this one whose bodies are well-formed are kept
// for the election; malformed ones take no place in the queue (PROTOCOL.md section 7.2), and a
// stray entry is found out when the election examines it. Bodies never change once published,
// so each is read once, when its name is first seen.
func (t *Ticket) rescan() error {
	entries, err := t.e.st.ListNames(store.QueueDir)
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(entries))
	var ahead []aheadEntry
	for _, en := range entries {
		present[en.Name] = true
		k := state.Key(en.ID)
		if !k.Less(t.key) {
			continue
		}
		ok, err := t.wellFormed(en.Name)
		if err != nil {
			return err
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
	slices.SortFunc(ahead, func(a, b aheadEntry) int {
		switch {
		case a.key.Less(b.key):
			return -1
		case b.key.Less(a.key):
			return 1
		}
		return 0
	})
	t.ahead = ahead
	return nil
}

// wellFormed reports whether the body of the ticket name in queue/ is well-formed, reading it
// only when the name is first seen: bodies never change once published, so the answer is kept in
// ticketOK. A stray entry under the name counts as malformed and is never trusted (PROTOCOL.md
// section 4.2). A ticket gone before its body was read is not well-formed, and that answer is not
// kept.
func (t *Ticket) wellFormed(name string) (bool, error) {
	if ok, known := t.ticketOK[name]; known {
		return ok, nil
	}
	body, err := t.e.readBody(store.QueueDir, name)
	if gone(err) {
		return false, nil
	}
	if err != nil && !stray(err) {
		return false, err
	}
	_, derr := wire.DecodeTicket(body)
	ok := err == nil && derr == nil
	t.ticketOK[name] = ok
	return ok, nil
}

// Holders lists holds/ and returns the live holds (a lock probe each); malformed holds are
// left out (PROTOCOL.md section 7.4). It is informative and not part of waiting: the CLI calls
// it for its waiting line every 30 s. Hold bodies are read once per name.
func (t *Ticket) Holders() ([]HolderInfo, error) {
	t.stepMu.Lock()
	defer t.stepMu.Unlock()
	if t.mode == modeTokenOnly {
		return nil, nil
	}
	holds, _, err := t.e.st.List(store.HoldsDir)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(holds))
	var live []HolderInfo
	for _, en := range holds {
		present[en.Name] = true
		info, known := t.holdBodies[en.Name]
		if !known {
			body, err := t.e.readBody(store.HoldsDir, en.Name)
			if gone(err) {
				continue
			}
			if err != nil && !stray(err) {
				return nil, err
			}
			if err == nil {
				info = holderInfo(en, body)
			}
			t.holdBodies[en.Name] = info
		}
		if info == nil {
			continue
		}
		dead, err := probe(t.e.st, store.HoldsDir, en.Name)
		if gone(err) || stray(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !dead {
			live = append(live, *info)
		}
	}
	for name := range t.holdBodies {
		if !present[name] {
			delete(t.holdBodies, name)
		}
	}
	return live, nil
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
