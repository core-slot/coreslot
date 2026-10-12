//go:build unix

package coreslot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/store"
)

// Client is an opened budget. It is safe for concurrent use.
type Client struct {
	e *engine.Engine
}

// Open opens the state directory: Options.StateDir when set (with its configuration at
// <StateDir>/config.toml), otherwise the CORESLOT_STATE_DIR override or the default of
// PROTOCOL.md section 4.1. An unusable directory is a *StateError.
func Open(o Options) (*Client, error) {
	dir, cfgPath := o.StateDir, ""
	if dir == "" {
		var err error
		if dir, cfgPath, err = config.Locate(); err != nil {
			return nil, &StateError{Reason: err.Error(), err: err}
		}
	} else {
		if !filepath.IsAbs(dir) {
			return nil, &StateError{Reason: "Options.StateDir " + dir + " is not an absolute path"}
		}
		dir = filepath.Clean(dir)
		cfgPath = filepath.Join(dir, store.ConfigFile)
	}
	st, err := store.Open(dir)
	if err != nil {
		return nil, wrap(err)
	}
	e := engine.New(st, &config.Watcher{Path: cfgPath}, config.MachineCPUs, engine.Clock{})
	return &Client{e: e}, nil
}

// wrap applies the error contract (engine.StateReason): every error but the classes a caller acts
// on becomes a *StateError wrapping it.
func wrap(err error) error {
	if reason, ok := engine.StateReason(err); ok {
		return &StateError{Reason: reason, err: err}
	}
	return err
}

// Enqueue validates req (an invalid one is a *UsageError) and publishes its ticket. The caller
// must then Poll at least every 2 s, or Wait: a Waiter not polled for 10 s becomes STALLED for
// the other clients and stops blocking them. Under a state directory format this client does
// not implement, the waiter waits in token-only mode and its grant is TokenOnly.
//
// Enqueue waits for the admission section as long as it takes; a process stopped inside the
// section blocks it until that process is continued or killed. EnqueueContext bounds the wait.
func (c *Client) Enqueue(req Request) (*Waiter, error) {
	return c.EnqueueContext(context.Background(), req)
}

// EnqueueContext is Enqueue whose wait for the admission section ends with ctx (PROTOCOL.md
// section 8: retried from 10 ms up to 250 ms). When ctx ends first nothing is published and the
// error matches ctx.Err(); if another client held the section, its text names the pid
// admit.lock records for it. Once inside the section the enqueue completes.
func (c *Client) EnqueueContext(ctx context.Context, req Request) (*Waiter, error) {
	t, err := c.e.EnqueueContext(ctx, req)
	if err != nil {
		return nil, wrap(err)
	}
	return &Waiter{t: t}, nil
}

// Adopt admits already-running work without queueing (PROTOCOL.md section 12.4), taking what
// is free up to each resource's Max. shortfall maps every requested declared resource to
// max(0, Min - granted): what the work runs without budget. Adopt waits for the admission
// section as long as it takes; AdoptContext bounds the wait.
func (c *Client) Adopt(req Request) (*Grant, map[string]int, error) {
	return c.AdoptContext(context.Background(), req)
}

// AdoptContext is Adopt whose wait for the admission section ends with ctx, as EnqueueContext's
// does: nothing is taken or published then.
func (c *Client) AdoptContext(ctx context.Context, req Request) (*Grant, map[string]int, error) {
	g, short, err := c.e.AdoptContext(ctx, req)
	if err != nil {
		return nil, nil, wrap(err)
	}
	return &Grant{g: g}, short, nil
}

// Free returns resource's size minus its locked tokens now; it never waits for the section. A
// resource the configuration does not declare matches ErrRefused.
func (c *Client) Free(resource string) (int, error) {
	n, err := c.e.Free(resource)
	return n, wrap(err)
}

// Snapshot reads the state directory without writing anything further: the data behind
// `status --json`. Open has already created the directory, res/ and admit.lock when they were
// missing; the coreslot command reads through a read-only open instead, which creates nothing.
func (c *Client) Snapshot() (Report, error) {
	r, err := c.e.Snapshot()
	return r, wrap(err)
}

// Waiter is a queued request. Its methods are safe for concurrent use: the first Poll or Wait
// to admit wins and every other returns the same *Grant; Cancel after the admission returns
// ErrAlreadyAdmitted; Cancel during Wait makes it return ErrCancelled.
type Waiter struct {
	t *engine.Ticket

	mu    sync.Mutex
	grant *Grant
}

func (w *Waiter) wrapGrant(g *engine.Grant) *Grant {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.grant == nil {
		w.grant = &Grant{g: g}
	}
	return w.grant
}

// Poll makes one non-blocking attempt (PROTOCOL.md section 8); call it at least every 2 s: it refreshes the ticket, checks
// for a cancel request and, when this waiter is the head, tries admission with a single try at
// the section. ok reports an admission. After an error the request is still queued: poll again,
// or Cancel to withdraw it.
func (w *Waiter) Poll() (g *Grant, ok bool, err error) {
	r, err := w.t.Poll()
	if err != nil {
		return nil, false, wrap(err)
	}
	if a, isAdmitted := r.(engine.Admitted); isAdmitted {
		return w.wrapGrant(a.Grant), true, nil
	}
	return nil, false, nil
}

// Wait polls until admitted. When ctx ends first it withdraws the ticket (it is dead at once
// to every other client) and returns ctx.Err(); a Cancel makes it return ErrCancelled. Any other
// error (a *StateError, for example) also withdraws the ticket before it is returned.
func (w *Waiter) Wait(ctx context.Context) (*Grant, error) {
	r, err := stepWhenDue(ctx, w.t)
	if a, ok := r.(engine.Admitted); ok && err == nil {
		return w.wrapGrant(a.Grant), nil
	}
	if err == nil || ctx.Err() != nil {
		return w.abandon(ctx)
	}
	if errors.Is(err, engine.ErrCancelled) {
		return nil, err
	}
	// Any other error ends the wait: withdraw, so that the ticket is dead at once instead of
	// blocking the queue unrefreshed until it stalls. An admission that won the race is
	// returned instead.
	if errors.Is(w.t.Withdraw(), engine.ErrAlreadyAdmitted) {
		if g, ok, perr := w.Poll(); ok && perr == nil {
			return g, nil
		}
	}
	return nil, wrap(err)
}

// stepper is the part of a ticket Wait drives.
type stepper interface {
	Step(ctx context.Context) (engine.StepResult, error)
	Due() time.Duration
}

// stepWhenDue Steps s at once and then whenever s next has work due (a waiter that is not the
// head: once per second; the head: on its admission backoff), until a Step admits or fails. It
// returns that Step's outcome, or nil and nil when ctx ended between Steps.
func stepWhenDue(ctx context.Context, s stepper) (engine.StepResult, error) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		case <-timer.C:
		}
		r, err := s.Step(ctx)
		if _, ok := r.(engine.Admitted); ok || err != nil {
			return r, err
		}
		timer.Reset(s.Due())
	}
}

// abandon withdraws the ticket for a Wait whose ctx ended; an admission that won the race is
// returned instead.
func (w *Waiter) abandon(ctx context.Context) (*Grant, error) {
	if err := w.t.Withdraw(); errors.Is(err, engine.ErrAlreadyAdmitted) {
		if g, ok, perr := w.Poll(); ok && perr == nil {
			return g, nil
		}
	}
	return nil, ctx.Err()
}

// Cancel withdraws the request by closing its ticket descriptor; it never waits for the
// section. It is idempotent before admission and returns ErrAlreadyAdmitted after it.
func (w *Waiter) Cancel() error {
	return wrap(w.t.Withdraw())
}

// Grant is what an admission holds. Its methods are safe for concurrent use.
type Grant struct {
	g *engine.Grant
}

// Grant maps every requested resource to what the command may use: the tokens held, or for a
// resource run unbudgeted, its requested Min.
func (g *Grant) Grant() map[string]int { return g.g.Grant() }

// Clamped maps each resource whose Min exceeded its whole size to that Min; such a grant holds
// the whole resource.
func (g *Grant) Clamped() map[string]int { return g.g.Clamped() }

// Unbudgeted lists, sorted, the requested resources the configuration does not declare.
func (g *Grant) Unbudgeted() []string { return g.g.Unbudgeted() }

// TokenOnly reports a token-only grant (an unknown state directory format): no hold, and Env
// carries CORESLOT_UNBUDGETED=format.
func (g *Grant) TokenOnly() bool { return g.g.TokenOnly() }

// Files are the descriptors for exec.Cmd.ExtraFiles: the hold as fd 3 (nil when there is none,
// so fd 3 stays closed), then the tokens from fd 4.
func (g *Grant) Files() []*os.File { return g.g.Files() }

// Env is the CORESLOT_* environment matching Files. Remove every inherited
// CORESLOT_GRANTED_*, CORESLOT_HELD_FD, CORESLOT_TOKEN_FDS and CORESLOT_UNBUDGETED from the
// command's environment before appending it (PROTOCOL.md section 17): inherited grant context
// describes another grant. CORESLOT_STATE_DIR is not grant context and is kept.
func (g *Grant) Env() []string { return g.g.Env() }

// Close closes this process's copies of the descriptors (never unlocking, so a child keeps the
// grant until it exits) and removes the hold when no child holds it, waiting at most 2 s for
// the section. It is idempotent. When the hold or a token is no longer the file at its path (the
// state directory was removed, or removed and recreated, while the grant was held) it closes the
// descriptors, writes nothing into the directory as it now exists, and returns a *StateError.
func (g *Grant) Close() error { return wrap(g.g.Close()) }
