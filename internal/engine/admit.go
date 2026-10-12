//go:build unix

package engine

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"

	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/state"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// admit is the head's admission attempt (PROTOCOL.md section 12.1 steps 1 to 7, with the
// re-election of step 5). It returns
// the grant when admitted, nil with no error when the head must keep waiting (the section was
// busy, not enough is free, or a non-queued taker won a race), and ErrCancelled when a cancel
// request was published before this section.
func (t *Ticket) admit(ctx context.Context, single bool) (*Grant, error) {
	point("admit.before-enter")
	sec, entered, err := t.e.enter(ctx, single)
	if err != nil || !entered {
		return nil, err
	}
	defer sec.Leave()
	_, understood, err := sec.Format(formats())
	if err != nil {
		return nil, err
	}
	if !understood {
		t.toTokenOnly()
		return nil, nil
	}

	// 1. A cancel request published before this section wins.
	cancelled, err := t.cancelRequested()
	if err != nil {
		return nil, err
	}
	if cancelled {
		if t.settle(statusCancelled) {
			t.removeOwn(sec)
		}
		return nil, ErrCancelled
	}

	// 2-6. Sizes, effective request, locked counts, the two passes.
	sizes, err := t.e.sizes()
	if err != nil {
		return nil, err
	}
	eff, clamped, undeclared := state.Effective(t.req.Need, sizes)
	locked := make(map[string]int, len(eff))
	for name := range eff {
		if locked[name], err = sec.CountLocked(name); err != nil {
			return nil, err
		}
	}
	take, ok := state.Plan(eff, sizes, locked)
	if !ok {
		return nil, nil
	}
	// 5. The re-election: a ticket published ahead of this one since its election wins.
	if ok, err := t.reelect(); err != nil || !ok {
		return nil, err
	}
	point("admit.planned")
	ts, ok, err := takeTwoPasses(sec, eff, take, sizes)
	if err != nil || !ok {
		return nil, err
	}
	point("tokens.taken")
	if err := ts.CheckIdentity(); err != nil {
		ts.Close()
		return nil, err
	}

	// 7. Publish the hold, close and unlink the ticket, bump gen. A Withdraw that came first
	// wins; once the hold is published the ticket is admitted and Withdraw has no effect.
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != statusWaiting {
		ts.Close()
		return nil, ErrCancelled
	}
	g := &Grant{e: t.e, tokens: ts, need: t.req.Need, clamped: clamped, unbudgeted: undeclared}
	if err := g.publishHold(sec, t.req.Label, false); err != nil {
		ts.Close()
		return nil, err
	}
	t.f.Close()
	_, _ = sec.RemoveOwnTicket(t.name) // already gone counts as success (PROTOCOL.md 10.3)
	// A failed bump does not undo the admission: waiters probe the tickets ahead of them on
	// every election, so the closed ticket is passed over without a rescan.
	_ = sec.BumpGen()
	t.status, t.grant = statusAdmitted, g
	return g, nil
}

// reelect is the re-election inside the admitting section (PROTOCOL.md section 12.1 step 5): it
// lists the names in queue/ again and reports whether every well-formed ticket ahead of this one
// is dead, gone or STALLED for this observer, reading the mtime (lstat) of only those tickets.
// A ticket this observer has never watched, such as one published after the election, counts as
// live; the next election then rescans whatever gen says, so a ticket published without a gen
// bump is watched from then on and cannot keep the head out for longer than a stall.
func (t *Ticket) reelect() (ok bool, err error) {
	entries, err := t.e.st.ListNames(store.QueueDir)
	if err != nil {
		return false, err
	}
	now := t.e.clock.Monotonic()
	for _, en := range entries {
		k := state.Key{OrderKey: en.ID.OrderKey, PID: en.ID.PID, Seq: en.ID.Seq}
		if !k.Less(t.key) {
			continue
		}
		well, known := t.ticketOK[en.Name]
		if !known {
			body, err := t.e.readBody(store.QueueDir, en.Name)
			if gone(err) {
				continue
			}
			if err != nil && !stray(err) {
				return false, err
			}
			_, derr := wire.DecodeTicket(body)
			well = err == nil && derr == nil
			t.ticketOK[en.Name] = well
		}
		if !well {
			continue
		}
		fi, err := lstat(filepath.Join(t.e.st.Dir(), store.QueueDir, en.Name))
		if gone(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		dead, err := probe(t.e.st, store.QueueDir, en.Name)
		if gone(err) || stray(err) || (err == nil && dead) {
			continue
		}
		if err != nil {
			return false, err
		}
		if _, seen := t.obs.Seen(k); !seen {
			t.haveGen = false // rescan at the next election, so that it is watched from then on
			return false, nil // first seen inside the section: live
		}
		t.obs.Observe(k, fi.ModTime().UnixNano(), now)
		if !t.obs.Stalled(k, now) {
			return false, nil
		}
	}
	return true, nil
}

// takeTwoPasses takes tokens in the two passes of PROTOCOL.md section 12.1 step 6: the
// minimum pass takes every effective min, and if any resource ends below it (a non-queued
// taker won a race) everything taken this round is closed and ok is false; the extra pass then
// takes take[r] - min more of each resource, as many as it finds free.
func takeTwoPasses(sec *store.Section, eff map[string]spec.Range, take, sizes map[string]int) (*store.TokenSet, bool, error) {
	mins := make(map[string]int, len(eff))
	extra := make(map[string]int, len(eff))
	for name, r := range eff {
		mins[name] = r.Min
		extra[name] = take[name] - r.Min
	}
	first, got, err := sec.TakeTokens(mins, sizes)
	if err != nil {
		return nil, false, err
	}
	for name, n := range mins {
		if got[name] < n {
			first.Close()
			return nil, false, nil
		}
	}
	second, _, err := sec.TakeTokens(extra, sizes)
	if err != nil {
		first.Close()
		return nil, false, err
	}
	return mergeTokens(first, second), true, nil
}

// mergeTokens joins two token sets into one ordered by resource name and then index, the order
// of the child's descriptors (PROTOCOL.md section 17).
func mergeTokens(sets ...*store.TokenSet) *store.TokenSet {
	type tok struct {
		name string
		idx  int
		f    *os.File
	}
	var all []tok
	for _, ts := range sets {
		k := 0
		for _, name := range slices.Sorted(maps.Keys(ts.Indices)) {
			for _, idx := range ts.Indices[name] {
				all = append(all, tok{name, idx, ts.Files[k]})
				k++
			}
		}
	}
	slices.SortFunc(all, func(a, b tok) int {
		if a.name != b.name {
			if a.name < b.name {
				return -1
			}
			return 1
		}
		return a.idx - b.idx
	})
	out := &store.TokenSet{Indices: map[string][]int{}}
	for _, x := range all {
		out.Files = append(out.Files, x.f)
		out.Indices[x.name] = append(out.Indices[x.name], x.idx)
	}
	return out
}

// Grant is what an admission, adoption or token-only admission holds: the hold's lock-carrying
// descriptor (none in token-only mode) and the token descriptors. Its methods are safe for
// concurrent use.
type Grant struct {
	e          *Engine
	hold       *os.File
	holdName   string
	tokens     *store.TokenSet
	need       map[string]spec.Range
	clamped    map[string]int
	unbudgeted []string
	tokenOnly  bool
	adopted    bool
	failOpen   string // the reason of a fail-open run's grant

	mu     sync.Mutex
	closed bool
}

// publishHold publishes the grant's hold inside sec and keeps its descriptor.
func (g *Grant) publishHold(sec *store.Section, label string, adopted bool) error {
	g.adopted = adopted
	start := max(g.e.clock.Realtime(), 0)
	h := wire.Hold{Label: label, Grant: g.Indices(), StartNS: uint64(start), Unbudgeted: g.failOpen != "",
		UnbudgetedResources: g.unbudgeted, Clamped: g.clamped, Adopted: adopted}
	f, name, err := sec.PublishHold(h, os.Getpid(), g.e.seq())
	if err != nil {
		return err
	}
	g.hold, g.holdName = f, name
	return nil
}

// Indices lists the token indices held, per resource; resources with none are left out.
func (g *Grant) Indices() map[string][]int {
	out := make(map[string][]int, len(g.tokens.Indices))
	for name, idx := range g.tokens.Indices {
		if len(idx) > 0 {
			out[name] = slices.Clone(idx)
		}
	}
	return out
}

// Grant maps every requested resource to what the command may use: the number of tokens held,
// or, for a resource run unbudgeted, its requested min (PROTOCOL.md section 17).
func (g *Grant) Grant() map[string]int {
	out := make(map[string]int, len(g.need))
	for name, r := range g.need {
		if slices.Contains(g.unbudgeted, name) {
			out[name] = r.Min
		} else {
			out[name] = len(g.tokens.Indices[name])
		}
	}
	return out
}

// Clamped maps each clamped resource to its requested min (PROTOCOL.md section 12.2).
func (g *Grant) Clamped() map[string]int { return maps.Clone(g.clamped) }

// Unbudgeted lists, sorted, the requested resources the configuration does not declare (every
// requested resource for a fail-open grant).
func (g *Grant) Unbudgeted() []string { return slices.Clone(g.unbudgeted) }

// TokenOnly reports a token-only grant: no hold, CORESLOT_UNBUDGETED=format.
func (g *Grant) TokenOnly() bool { return g.tokenOnly }

// Adopted reports an adoption's grant.
func (g *Grant) Adopted() bool { return g.adopted }

// HoldName is the hold's file name in holds/; empty without a hold.
func (g *Grant) HoldName() string { return g.holdName }

// Files are the descriptors for a child, for exec.Cmd.ExtraFiles: the hold first (fd 3), nil
// when there is none so that fd 3 stays closed, then the tokens from fd 4 in ascending resource
// name and index (PROTOCOL.md section 17).
func (g *Grant) Files() []*os.File {
	files := make([]*os.File, 0, 1+len(g.tokens.Files))
	files = append(files, g.hold) // nil without a hold
	return append(files, g.tokens.Files...)
}

// Env is the CORESLOT_* environment matching Files; the caller removes every inherited
// CORESLOT_GRANTED_*, CORESLOT_HELD_FD, CORESLOT_TOKEN_FDS and CORESLOT_UNBUDGETED before
// appending it (PROTOCOL.md section 17). It holds CORESLOT_GRANTED_<RES> for every requested
// resource, CORESLOT_HELD_FD=3 with a hold, CORESLOT_TOKEN_FDS when tokens are passed, and
// CORESLOT_UNBUDGETED=format in token-only mode (the reason for a fail-open grant).
func (g *Grant) Env() []string {
	grant := g.Grant()
	var env []string
	for _, name := range slices.Sorted(maps.Keys(grant)) {
		env = append(env, "CORESLOT_GRANTED_"+spec.EnvName(name)+"="+strconv.Itoa(grant[name]))
	}
	if g.hold != nil {
		env = append(env, "CORESLOT_HELD_FD=3")
	}
	switch n := len(g.tokens.Files); {
	case n == 1:
		env = append(env, "CORESLOT_TOKEN_FDS=4")
	case n > 1:
		env = append(env, "CORESLOT_TOKEN_FDS=4-"+strconv.Itoa(3+n))
	}
	switch {
	case g.tokenOnly:
		env = append(env, "CORESLOT_UNBUDGETED=format")
	case g.failOpen != "":
		env = append(env, "CORESLOT_UNBUDGETED="+g.failOpen)
	}
	return env
}

// Close closes this process's copies of the grant's descriptors, the hold before the tokens; it
// never unlocks, so a child that inherited copies keeps the grant until it exits. Then, if it can
// enter the section within 2 s, it probes the hold and removes it only when the probe succeeds
// (PROTOCOL.md section 12.3); otherwise reconciliation removes it later.
//
// Before closing it checks I1 for the hold and every token: each must still be linked and its
// path must name the same file. When one is not (the state directory was removed, or removed and
// recreated, under the holder), the holder reports it and writes nothing into the directory as it
// now exists: it closes the descriptors, removes no hold, enters no section, and returns the
// mismatch as a *store.StateError. It is idempotent and otherwise returns the first error from
// closing a descriptor.
func (g *Grant) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.mu.Unlock()
	var first error
	keep := func(err error) {
		if err != nil && first == nil && !errors.Is(err, os.ErrClosed) {
			first = err
		}
	}
	identity := g.checkHeld()
	// The hold first: a live hold must never list a token nobody locks (I1, I2). Tokens still
	// locked under a dead hold are the window I2 allows until the next reconciliation.
	if g.hold != nil {
		keep(g.hold.Close())
	}
	point("grant.after-hold-close")
	for _, f := range g.tokens.Files {
		keep(f.Close())
	}
	if identity != nil {
		return identity
	}
	if g.hold != nil {
		g.removeHold()
	}
	return first
}

// checkHeld checks I1 for the grant's hold and tokens before Close closes them.
func (g *Grant) checkHeld() error {
	if g.hold != nil {
		if err := store.CheckHeld(g.hold, filepath.Join(g.e.st.Dir(), store.HoldsDir, g.holdName)); err != nil {
			return err
		}
	}
	return g.tokens.CheckIdentity()
}

// removeHold is the best-effort removal of a closed grant's hold.
func (g *Grant) removeHold() {
	ctx, cancel := context.WithTimeout(context.Background(), releaseWait)
	defer cancel()
	sec, err := enterSection(g.e.st, ctx)
	if err != nil {
		return
	}
	defer sec.Leave()
	if _, understood, err := sec.Format(formats()); err != nil || !understood {
		return
	}
	if _, err := sec.ReconcileIfDue(g.e.clock.Realtime()); err != nil {
		return
	}
	if removed, err := sec.ProbeAndRemoveHold(g.holdName); err == nil && removed {
		_ = sec.BumpGen() // removing one's own hold bumps gen once (PROTOCOL.md section 7.8)
	}
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
