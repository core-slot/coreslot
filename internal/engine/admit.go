//go:build unix

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// staleNS is the age past which a stored machine sample is no reading: three periods.
const staleNS = int64(6 * time.Second)

// fresh reports whether a reading taken at atNS, by the wall clock, is still one at nowNS.
func fresh(atNS, nowNS int64) bool { return atNS > 0 && atNS <= nowNS && nowNS-atNS <= staleNS }

// admit is the admission attempt of the head (PROTOCOL.md section 12.1 steps 1 to 7). It returns
// the grant when admitted, nil with no error when the ticket must keep waiting (the section was
// busy, a ticket ahead was published since the election, or the live holds leave too little), and
// ErrCanceled when a cancel request was published before this section.
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

	// 2-4. The room: the headroom less the reserve and what the live holds still have to grow
	// into. With no hold live the head starts whatever the numbers say; a headroom that cannot be
	// read gates nothing.
	cfg, m, err := t.e.reading()
	if err != nil {
		return nil, err
	}
	holds, err := liveHolds(t.e.st, spec.ColdStartMiB(m.MemMiB))
	if err != nil {
		return nil, err
	}
	need := t.req.NeedMiB
	room := state.Room(m.HeadroomMiB, cfg.ReserveMiB, reservations(holds))
	if m.HeadroomOK && len(holds) > 0 && need > room {
		return nil, nil
	}
	// 5. The re-election: a ticket published ahead of this one since its election wins.
	if ahead, err := t.reelect(); err != nil || ahead {
		return nil, err
	}
	if len(holds) > 0 {
		if wait, err := t.watchWait(sec, cfg, m, need, room); wait || err != nil {
			return nil, err
		}
	}

	// 7. Publish the hold, close and unlink the ticket, bump gen. A Withdraw that came first
	// wins; once the hold is published the ticket is admitted and Withdraw has no effect.
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != statusWaiting {
		return nil, ErrCanceled
	}
	g := &Grant{e: t.e, grantMiB: need}
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

// watchWait reports whether the shared watch state keeps the ticket out while holds are live. The
// detector's times are on the sample's clock, which the sample's age carries to now. A restart
// takes its part in the tick, so that calm is measured even when no run ticks.
func (t *Ticket) watchWait(sec *store.Section, cfg config.Config, m config.Machine, need, room int64) (bool, error) {
	c := cfg.Watch
	if t.req.Restart {
		_, _, _ = sec.Tick(t.e.clock.Realtime(), t.e.sampler(cfg, m, nil), c) // a failed sample is Unknown
	}
	d, s, at, err := sec.Watch()
	now := t.e.clock.Realtime()
	ok := fresh(at, now)
	mono, spacing := s.Host.WallNS+now-at, int64(c.SpacingS*1e9)
	switch {
	case err != nil:
		return false, err
	case t.req.Restart && (!ok || d.Level != watch.Green || mono-d.CalmSinceNS < int64(c.CalmS*1e9) ||
		m.HeadroomOK && room < need+max(256, need/10)):
		return true, nil
	case !ok || d.Level == watch.Unknown:
		return false, nil
	case d.Level >= watch.Yellow:
		return true, nil
	// Spacing needs the clear and the last start on this sample's clock: a reboot keeps watch.json.
	case s.Host.WallNS == 0 || d.YellowClearedNS > mono || d.LastAdmitNS > mono ||
		mono-d.YellowClearedNS >= int64(c.CalmS*1e9): // no clear within calm_s: no spacing
	case d.LastAdmitNS >= d.YellowClearedNS && mono-d.LastAdmitNS < spacing:
		return true, nil
	default:
		d.LastAdmitNS = mono
		_ = sec.SetWatch(d, s, at) // a start it could not record only spaces the next less
	}
	return false, nil
}

// reelect is the re-election inside the admitting section (PROTOCOL.md section 12.1 step 5): it
// lists the names in queue/ again and reports whether a live ticket is ahead of this one:
// well-formed and neither gone, dead nor STALLED for this observer, reading the mtime (lstat) of
// only well-formed tickets. A ticket this observer has never watched, such as one published after
// the election, counts as live; the next election then rescans whatever gen says, so a ticket
// published without a gen bump is watched from then on and cannot keep the head out for longer
// than a stall.
func (t *Ticket) reelect() (ahead bool, err error) {
	entries, err := t.e.st.ListNames(store.QueueDir)
	if err != nil {
		return false, err
	}
	now := t.e.clock.Monotonic()
	for _, en := range entries {
		k := state.Key(en.ID)
		if !k.Less(t.key) {
			continue
		}
		ok, err := t.ticketBody(en.Name)
		if err != nil {
			return false, err
		}
		if !ok {
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
		if gone(err) || stray(err) || err == nil && dead {
			continue
		}
		if err != nil {
			return false, err
		}
		if _, seen := t.obs.Seen(k); !seen {
			t.haveGen = false // rescan at the next election, so that it is watched from then on
		} else {
			t.obs.Observe(k, fi.ModTime().UnixNano(), now)
			if t.obs.Stalled(k, now) {
				continue
			}
		}
		return true, nil
	}
	return false, nil
}

// Grant is what an admission holds: the hold's lock-carrying descriptor, which carries the
// reservation in its body. Its methods are safe for concurrent use.
type Grant struct {
	e        *Engine
	hold     *os.File
	holdName string
	grantMiB int64  // the need the command was granted
	failOpen string // the reason of a fail-open run's grant

	mu     sync.Mutex
	body   wire.Hold // the hold's body as last written
	closed bool
}

// publishHold publishes the grant's hold for req, which arrived at arrivalNS, reserving needMiB,
// inside sec and keeps its descriptor.
func (g *Grant) publishHold(sec *store.Section, req spec.Request, arrivalNS, needMiB int64) error {
	start := max(g.e.clock.Realtime(), 0)
	h := wire.Hold{Label: req.Label, PID: os.Getpid(), StartNS: start, ArrivalNS: max(arrivalNS, 0),
		NeedMiB: needMiB}
	f, name, err := sec.PublishHold(h, g.e.seq())
	if err != nil {
		return err
	}
	g.hold, g.holdName, g.body = f, name, h
	return nil
}

// GrantMiB is the memory the command was granted, in MiB: the need.
func (g *Grant) GrantMiB() int64 { return g.grantMiB }

// ArrivalNS is the wall clock at the run's first arrival in the queue, as its hold carries it.
func (g *Grant) ArrivalNS() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.body.ArrivalNS
}

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
