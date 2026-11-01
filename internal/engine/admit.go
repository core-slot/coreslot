//go:build unix

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// admit is the admission attempt of the head, or of a waiter in the front window that may pass
// the live tickets ahead (PROTOCOL.md section 12.1 steps 1 to 7, with the re-election of step 5,
// which decides which of the two it is). It returns the grant when admitted, nil with no error
// when the ticket must keep waiting (the section was busy, or the live holds leave too little),
// and ErrCanceled when a cancel request was published before this section.
func (t *Ticket) admit(ctx context.Context, single bool) (*Grant, error) {
	point("admit.before-enter")
	sec, entered, err := t.e.enter(ctx, single)
	if err != nil || !entered {
		return nil, err
	}
	defer sec.Leave()
	n, understood, err := sec.Format()
	if err != nil {
		return nil, err
	}
	if !understood {
		return nil, refuseFormat(n, "")
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
		return nil, ErrCanceled
	}

	// 2-4. The capacity, and what the live holds reserve of it. A need above the capacity is
	// clamped to it and admitted only when nothing is reserved.
	sizeMiB, live, err := t.e.memSize()
	if err != nil {
		return nil, err
	}
	holds, err := liveHolds(t.e.st)
	if err != nil {
		return nil, err
	}
	need := t.req.NeedMiB
	reserved := reservedMiB(holds)
	capacity, raised := capacityFor(sizeMiB, live, holds, reserved, need)
	grant := min(need, capacity)
	if reserved+grant > capacity {
		return nil, nil
	}
	// 5. The re-election: a ticket published ahead of this one since its election wins, unless
	// this one may pass it: from the front window, with every body ahead well-formed, leaving
	// every need ahead free (section 10.3).
	ahead, front, malformed, err := t.reelect()
	if err != nil {
		return nil, err
	}
	if len(ahead) > 0 {
		// A passer sized from the live capacity does not pass: the needs ahead are counted
		// against the configured size, which the live capacity would let it take from.
		if front >= FrontWindow || malformed || raised {
			return nil, nil
		}
		for _, a := range ahead {
			reserved += min(a, capacity)
		}
		if reserved+grant > capacity {
			return nil, nil
		}
	}

	// 7. Publish the hold, close and unlink the ticket, bump gen. A Withdraw that came first
	// wins; once the hold is published the ticket is admitted and Withdraw has no effect.
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != statusWaiting {
		return nil, ErrCanceled
	}
	g := &Grant{e: t.e, grantMiB: grant, liveMem: raised}
	if grant < need {
		g.clampedMiB = need
	}
	if err := g.publishHold(sec, t.req, t.arrivalNS, need); err != nil {
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
// lists the names in queue/ again and returns, in ahead, the need of every live ticket ahead of
// this one: well-formed and neither gone, dead nor STALLED for this observer, reading the mtime
// (lstat) of only well-formed tickets. A ticket this observer has never watched, such as one
// published after the election, counts as live; the next election then rescans whatever gen
// says, so a ticket published without a gen bump is watched from then on and cannot keep the
// head out for longer than a stall. front counts the well-formed tickets ahead that are not
// gone, and malformed reports one ahead whose body is malformed or that a stray entry replaced.
// It stops once a live ticket is found with front at FrontWindow: the ticket can then neither be
// the head nor pass.
func (t *Ticket) reelect() (ahead []int64, front int, malformed bool, err error) {
	entries, err := t.e.st.ListNames(store.QueueDir)
	if err != nil {
		return nil, 0, false, err
	}
	now := t.e.clock.Monotonic()
	for _, en := range entries {
		if len(ahead) > 0 && front >= FrontWindow {
			break
		}
		k := state.Key(en.ID)
		if !k.Less(t.key) {
			continue
		}
		need, bad, err := t.ticketBody(en.Name)
		if err != nil {
			return nil, 0, false, err
		}
		malformed = malformed || bad
		if need == 0 {
			continue
		}
		fi, err := lstat(filepath.Join(t.e.st.Dir(), store.QueueDir, en.Name))
		if gone(err) {
			continue
		}
		if err != nil {
			return nil, 0, false, err
		}
		dead, err := probe(t.e.st, store.QueueDir, en.Name)
		if stray(err) {
			malformed = true
			continue
		}
		if gone(err) {
			continue
		}
		if err != nil {
			return nil, 0, false, err
		}
		front++ // a dead ticket not yet removed takes a place in the front window
		if dead {
			continue
		}
		if _, seen := t.obs.Seen(k); !seen {
			t.haveGen = false // rescan at the next election, so that it is watched from then on
		} else {
			t.obs.Observe(k, fi.ModTime().UnixNano(), now)
			if t.obs.Stalled(k, now) {
				continue
			}
		}
		ahead = append(ahead, need)
	}
	return ahead, front, malformed, nil
}

// Grant is what an admission holds: the hold's lock-carrying descriptor, which carries the
// reservation in its body. Its methods are safe for concurrent use.
type Grant struct {
	e          *Engine
	hold       *os.File
	holdName   string
	grantMiB   int64  // what the command may use
	clampedMiB int64  // the need asked when the capacity clamped it, else 0
	liveMem    bool   // the attempt that admitted it used a raised capacity
	failOpen   string // the reason of a fail-open run's grant

	mu     sync.Mutex
	body   wire.Hold // the hold's body as last written
	closed bool
}

// publishHold publishes the grant's hold for req, which arrived at arrivalNS, reserving needMiB,
// inside sec and keeps its descriptor.
func (g *Grant) publishHold(sec *store.Section, req spec.Request, arrivalNS, needMiB int64) error {
	start := max(g.e.clock.Realtime(), 0)
	h := wire.Hold{Label: req.Label, PID: os.Getpid(), StartNS: start, ArrivalNS: max(arrivalNS, 0),
		NeedMiB: needMiB, Tree: req.Tree}
	f, name, err := sec.PublishHold(h, g.e.seq())
	if err != nil {
		return err
	}
	g.hold, g.holdName, g.body = f, name, h
	return nil
}

// GrantMiB is the memory the command may use, in MiB: the need, or the capacity that clamped it.
func (g *Grant) GrantMiB() int64 { return g.grantMiB }

// ClampedMiB is the need asked when the capacity clamped it (PROTOCOL.md section 12.2), else 0.
func (g *Grant) ClampedMiB() int64 { return g.clampedMiB }

// LiveMem reports that the attempt that admitted g used a raised capacity.
func (g *Grant) LiveMem() bool { return g.liveMem }

// HoldName is the hold's file name in holds/; empty without a hold.
func (g *Grant) HoldName() string { return g.holdName }

// Files are the descriptors for a child, for exec.Cmd.ExtraFiles: the hold (fd 3), nil when there
// is none so that fd 3 stays closed (PROTOCOL.md section 17).
func (g *Grant) Files() []*os.File { return []*os.File{g.hold} }

// Env is the CORESLOT_* environment matching Files; the caller removes every inherited context
// variable (IsContext) before appending it (PROTOCOL.md section 17).
func (g *Grant) Env() []string { return ChildEnv(g.grantMiB, g.holdName, g.failOpen) }

// UpdateHold rewrites the grant's hold body with what fn changes in it, in place and without the
// section (store.RewriteHold); the label, pid and start time the name carries stay. It fails once
// the grant is closed or when it has no hold.
func (g *Grant) UpdateHold(fn func(*wire.Hold)) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.hold == nil {
		return errors.New("engine: the grant holds no hold to update")
	}
	h := g.body
	fn(&h)
	h.Label, h.PID, h.StartNS = g.body.Label, g.body.PID, g.body.StartNS
	if err := store.RewriteHold(g.hold, filepath.Join(g.e.st.Dir(), store.HoldsDir, g.holdName), h); err != nil {
		return err
	}
	g.body = h
	return nil
}

// Close closes this process's copy of the hold's descriptor; it never unlocks, so a child that
// inherited a copy keeps the grant until it exits. Then, if it can enter the section within 2 s,
// it probes the hold and removes it only when the probe succeeds (PROTOCOL.md section 12.3);
// otherwise reconciliation removes it later.
//
// Before closing it checks I1 for the hold: it must still be linked and its path must name the
// same file. When it is not (the state directory was removed, or removed and recreated, under the
// holder), the holder reports it and writes nothing into the directory as it now exists: it
// closes the descriptor, removes no hold, enters no section, and returns the mismatch as an
// *IdentityError around a *store.StateError. It is idempotent and otherwise returns the error
// from closing the descriptor.
func (g *Grant) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.mu.Unlock()
	if g.hold == nil {
		return nil
	}
	identity := store.CheckHeld(g.hold, filepath.Join(g.e.st.Dir(), store.HoldsDir, g.holdName))
	err := g.hold.Close()
	if identity != nil {
		return &IdentityError{Err: identity}
	}
	g.removeHold()
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
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
	if _, understood, err := sec.Prepare(g.e.clock.Realtime()); err != nil || !understood {
		return
	}
	if removed, err := sec.ProbeAndRemoveHold(g.holdName); err == nil && removed {
		_ = sec.BumpGen() // removing one's own hold bumps gen once (PROTOCOL.md section 7.8)
	}
}
