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

// admit is the head's admission attempt (PROTOCOL.md section 12.1 steps 1 to 7). It returns
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
	n, understood, err := sec.Format(formats())
	if err != nil {
		return nil, err
	}
	if !understood {
		return nil, t.onUnknownFormat(n)
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
	ts, ok, err := takeTwoPasses(sec, eff, take, sizes)
	if err != nil || !ok {
		return nil, err
	}
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
	_ = os.Remove(filepath.Join(t.e.st.Dir(), store.QueueDir, t.name))
	// A failed bump does not undo the admission: waiters probe the tickets ahead of them on
	// every election, so the closed ticket is passed over without a rescan.
	_ = sec.BumpGen()
	t.status, t.grant = statusAdmitted, g
	return g, nil
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

	mu     sync.Mutex
	closed bool
}

// publishHold publishes the grant's hold inside sec and keeps its descriptor.
func (g *Grant) publishHold(sec *store.Section, label string, adopted bool) error {
	g.adopted = adopted
	start := max(g.e.clock.Realtime(), 0)
	h := wire.Hold{Label: label, Grant: g.Indices(), StartNS: uint64(start),
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

// Unbudgeted lists, sorted, the requested resources the configuration does not declare.
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

// Env is the CORESLOT_* environment matching Files: CORESLOT_GRANTED_<RES> for every requested
// resource, CORESLOT_HELD_FD=3 with a hold, CORESLOT_TOKEN_FDS when tokens are passed, and
// CORESLOT_UNBUDGETED=format in token-only mode.
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
	if g.tokenOnly {
		env = append(env, "CORESLOT_UNBUDGETED=format")
	}
	return env
}

// Close closes this process's copies of the grant's descriptors; it never unlocks, so a child
// that inherited copies keeps the grant until it exits. Then, if it can enter the section
// within 2 s, it probes the hold and removes it only when the probe succeeds (PROTOCOL.md
// section 12.3); otherwise reconciliation removes it later. It is idempotent and returns the
// first error from closing a descriptor.
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
	for _, f := range g.tokens.Files {
		keep(f.Close())
	}
	if g.hold != nil {
		keep(g.hold.Close())
		g.removeHold()
	}
	return first
}

// removeHold is the best-effort removal of a closed grant's hold.
func (g *Grant) removeHold() {
	ctx, cancel := context.WithTimeout(context.Background(), releaseWait)
	defer cancel()
	sec, err := g.e.st.Enter(ctx)
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
