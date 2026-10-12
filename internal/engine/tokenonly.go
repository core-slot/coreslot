//go:build unix

package engine

import (
	"context"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/state"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// toTokenOnly switches a waiting ticket to token-only mode after a section found a FORMAT this
// client does not implement (PROTOCOL.md section 15.2): it withdraws the ticket by closing its
// descriptor, writing nothing, so the ticket is dead at once and reconciliation removes it.
func (t *Ticket) toTokenOnly() {
	t.mu.Lock()
	if t.f != nil {
		t.f.Close()
		t.f = nil
	}
	t.name, t.id = "", wire.ID{}
	t.mu.Unlock()
	t.mode = modeTokenOnly
	t.ahead, t.obs, t.haveGen, t.needBump = nil, state.Observer{}, false, false
	t.last = Waiting{}
	t.nextElection, t.backoff = 0, 0
}

// stepTokenOnly is one Step in token-only mode: at most once per head backoff (0.25 s up to
// 2 s) it enters the section and decides the mode from the FORMAT read there. A format this
// client implements, or no FORMAT (which the entry writes), ends token-only mode: the request
// enqueues in that section as a new one, with a fresh arrival (PROTOCOL.md section 15.2).
// Otherwise it tries token-only admission.
func (t *Ticket) stepTokenOnly(ctx context.Context, single bool, now time.Duration) (StepResult, error) {
	if now < t.nextElection {
		return t.last, nil
	}
	t.attempts++
	g, err := t.tokenOnlyAttempt(ctx, single)
	if err != nil {
		return t.settledOr(err)
	}
	if g != nil {
		return Admitted{g}, nil
	}
	if t.mode == modeQueued {
		return t.last, nil // enqueued: the next Step waits in the queue
	}
	if t.backoff == 0 {
		t.backoff = HeadFirstBackoff
	} else {
		t.backoff = min(2*t.backoff, HeadMaxBackoff)
	}
	t.nextElection = now + t.backoff
	return t.last, nil
}

// tokenOnlyAttempt is one token-only admission (PROTOCOL.md section 15.2): sizes from this
// client's own configuration, locked tokens counted at every index, and, if every declared
// resource's effective min is free, exactly that min of each taken in ascending resource name
// and index. It publishes nothing and writes nothing but missing token files and their
// directories: no ticket, hold, gen bump, log, degraded record or admit.lock body.
func (t *Ticket) tokenOnlyAttempt(ctx context.Context, single bool) (*Grant, error) {
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
	if understood {
		t.mu.Lock()
		waiting := t.status == statusWaiting
		t.mu.Unlock()
		if !waiting {
			return nil, ErrCancelled
		}
		if err := t.publish(sec); err != nil {
			return nil, err
		}
		t.last = Waiting{Position: 1}
		t.nextElection, t.backoff = 0, 0
		return nil, nil
	}

	sizes, err := t.e.sizes()
	if err != nil {
		return nil, err
	}
	eff, clamped, undeclared := state.Effective(t.req.Need, sizes)
	locked, err := countLocked(sec, eff)
	if err != nil {
		return nil, err
	}
	if _, ok := state.Plan(eff, sizes, locked); !ok {
		return nil, nil
	}
	ts, ok, err := takeMins(sec, eff, sizes)
	if err != nil || !ok {
		return nil, err
	}
	point("tokens.taken")
	if err := ts.CheckIdentity(); err != nil {
		ts.Close()
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != statusWaiting {
		ts.Close()
		return nil, ErrCancelled
	}
	g := &Grant{e: t.e, tokens: ts, need: t.req.Need, clamped: clamped, unbudgeted: undeclared, tokenOnly: true}
	t.status, t.grant = statusAdmitted, g
	return g, nil
}
