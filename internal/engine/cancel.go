//go:build unix

package engine

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// CancelContext publishes a cancel request for the ticket id inside the section (PROTOCOL.md
// section 13.1); the ticket's owner finds it on its next poll or at admission step 1 and stops.
// Canceling never signals and never revokes a grant: when no ticket with that id is in the
// queue (admitted, withdrawn, removed) nothing is published and it returns ErrNoTicket. A
// request already published means the cancellation is already requested: not an error. In a
// state directory whose format this client does not implement nothing is written and it
// returns an error matching ErrRefused. Its wait for the section ends with ctx, as
// EnqueueContext's does: it then publishes nothing and returns a *SectionBusyError naming the
// pid admit.lock records and matching ctx.Err() (context.DeadlineExceeded for a deadline), or
// ctx.Err() itself when ctx had already ended.
func (e *Engine) CancelContext(ctx context.Context, id wire.ID) error {
	sec, err := e.enterUntil(ctx)
	if err != nil {
		return err
	}
	defer sec.Leave()
	now := e.clock.Realtime() // reconciliation and the cancel request read the same clock
	n, understood, err := sec.Prepare(now)
	if err != nil {
		return err
	}
	if !understood {
		return refuseFormat(n, "")
	}
	entries, _, err := e.st.List(store.QueueDir)
	if err != nil {
		return err
	}
	found := false
	for _, en := range entries {
		if en.ID == id {
			found = true
			break
		}
	}
	if !found {
		return ErrNoTicket
	}
	body, err := wire.Encode(wire.Cancel{RequestedNS: max(now, 0), RequesterPID: os.Getpid()})
	if err != nil {
		return err
	}
	// An existing request means the cancellation is already requested, which is not an error
	// (PROTOCOL.md section 11). Publishing a cancel request does not bump gen (section 7.8).
	if _, err := sec.Publish(store.CancelDir, id.String(), body, false); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// cancelRequested reports whether a well-formed cancel request for this ticket exists. A
// malformed one is ignored, never trusted (PROTOCOL.md section 7.2).
func (t *Ticket) cancelRequested() (bool, error) {
	body, err := t.e.readBody(store.CancelDir, t.id.String())
	if gone(err) || stray(err) {
		return false, nil // a stray entry under the id is ignored, never trusted
	}
	if err != nil {
		return false, err
	}
	_, err = wire.DecodeCancel(body)
	return err == nil, nil
}

// onCancelled handles a cancel request found on a poll: the ticket descriptor is closed (the
// ticket is dead at once), the ticket and the request are removed if a section can be entered
// at once, and the result is ErrCanceled.
func (t *Ticket) onCancelled() error {
	if !t.settle(statusCancelled) {
		_, err := t.settledOr(nil)
		return err
	}
	t.removeOwn(nil)
	return ErrCanceled
}

// settle moves a waiting ticket to a final non-admitted status and closes its descriptor. It
// reports false when the ticket was no longer waiting.
func (t *Ticket) settle(s status) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != statusWaiting {
		return false
	}
	t.status = s
	if t.f != nil {
		t.f.Close()
	}
	return true
}

// Withdraw withdraws a waiting ticket (PROTOCOL.md section 13.2): it closes the ticket
// descriptor, so the ticket is dead at once to every observer, without entering the section.
// Then, only if the section can be entered at once, it removes the ticket file; otherwise
// reconciliation does. Every later Step returns ErrCanceled. Withdrawing an admitted ticket
// returns ErrAlreadyAdmitted and changes nothing; withdrawing again is a no-op.
func (t *Ticket) Withdraw() error {
	t.mu.Lock()
	switch t.status {
	case statusAdmitted:
		t.mu.Unlock()
		return ErrAlreadyAdmitted
	case statusWaiting:
		t.status = statusWithdrawn
		if t.f != nil {
			t.f.Close()
		}
		t.mu.Unlock()
	default:
		t.mu.Unlock()
		return nil
	}
	t.removeOwn(nil)
	return nil
}

// removeOwn removes this ticket's file and any cancel request for it, inside sec (which the
// caller entered), or, when sec is nil, inside a section entered with a single attempt (giving up if it is busy). The ticket
// descriptor is already closed, so the file is dead. Removal is best-effort: what stays behind
// is dead and reconciliation removes it.
func (t *Ticket) removeOwn(sec *store.Section) {
	name, id := t.ownName()
	if name == "" {
		return
	}
	if sec == nil {
		s, ok, err := tryEnterSection(t.e.st)
		if err != nil || !ok {
			return
		}
		defer s.Leave()
		if _, understood, err := s.Prepare(t.e.clock.Realtime()); err != nil || !understood {
			return
		}
		sec = s
	}
	// A ticket already gone counts as success: only a non-conforming actor removes a live
	// ticket, and then there is nothing left to remove (PROTOCOL.md section 10.3).
	removed, _ := sec.RemoveOwnTicket(name)
	_, _ = sec.RemoveCancel(id)
	if removed {
		// Removing one's own ticket bumps gen once (PROTOCOL.md section 7.8).
		_ = sec.BumpGen()
	}
}

// ownName returns the ticket's name and id under mu, not stepMu, which a concurrent Step may
// hold: every write of them holds both.
func (t *Ticket) ownName() (string, wire.ID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.name, t.id
}
