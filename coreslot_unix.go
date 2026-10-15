//go:build unix

package coreslot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
)

// Client is an opened budget. It is safe for concurrent use. Create one with [Open]: the zero
// Client is not usable.
type Client struct {
	e           *engine.Engine
	independent bool
	named       bool // opened without Options.StateDir: on the directory the environment names
}

// Open opens the state directory: Options.StateDir when set (with its configuration at
// <StateDir>/config.toml), otherwise the CORESLOT_STATE_DIR override or the default of
// PROTOCOL.md section 4.1. An unusable directory is a [*StateError]; an Options.StateDir that is
// not an absolute path is the caller's own mistake, a [*UsageError].
//
// Open locates and opens the directory before any request, so inside another run's grant whose
// parent runs unbudgeted because the directory is unusable, Open returns that *StateError rather
// than follow the parent; a caller that wants to follow it reads CORESLOT_UNBUDGETED.
//
// The default comes from the user's home directory in the password database. When that lookup
// reports no record for the user, or finds no /etc/passwd, Open asks NSS by running getent from a
// fixed system path as a short-lived child process, and waits at most 5 s for each run of it.
// That matters most to a build without cgo for Linux or another Unix but macOS, which reads
// /etc/passwd alone and so misses an account only NSS knows (LDAP, SSSD, systemd-homed); a cgo
// build has asked NSS already, and getent asks it a second time. Enqueue looks the home
// directory up the same way inside another run's grant when CORESLOT_STATE_DIR is unset. Like
// any child the caller's process starts, getent inherits the environment and every descriptor
// that is not close-on-exec. A caller that sets SIGCHLD to SIG_IGN or reaps every child (a PID 1
// calling wait4(-1)) can make the wait for getent fail, and Open then returns a *StateError; such
// a caller names the directory in Options.StateDir or CORESLOT_STATE_DIR, which also skips the
// lookup, and its wait, entirely.
func Open(o Options) (*Client, error) {
	dir, cfgPath := o.StateDir, ""
	if dir == "" {
		var err error
		if dir, cfgPath, err = config.Locate(); err != nil {
			return nil, &StateError{Reason: err.Error(), err: err}
		}
	} else {
		if !filepath.IsAbs(dir) {
			return nil, &UsageError{Msg: fmt.Sprintf("Options.StateDir %q is not an absolute path", dir)}
		}
		dir = filepath.Clean(dir)
		cfgPath = filepath.Join(dir, store.ConfigFile)
	}
	st, err := store.Open(dir)
	if err != nil {
		return nil, wrap(err)
	}
	e := engine.New(st, &config.Watcher{Path: cfgPath}, config.MachineCPUs, engine.Clock{})
	return &Client{e: e, independent: o.Independent, named: o.StateDir == ""}, nil
}

// Close releases what the Client holds. It holds nothing today and returns nil, but a later
// version may hold a descriptor, so call it once the Client is no longer needed, and do not use
// the Client after it. It does not end the Client's Waiters or Grants: cancel and close those on
// their own.
func (c *Client) Close() error { return nil }

// wrap applies the error contract (engine.StateReason) at the package boundary: every error but
// the classes a caller acts on becomes a *StateError wrapping it, and the engine's usage and
// section errors become this package's.
func wrap(err error) error {
	var (
		usage *spec.UsageError
		busy  *engine.SectionBusyError
	)
	switch {
	case errors.As(err, &usage):
		return &UsageError{Msg: err.Error()}
	case errors.As(err, &busy):
		return &SectionBusyError{PID: busy.PID, Err: busy.Err}
	}
	if reason, ok := engine.StateReason(err); ok {
		return &StateError{Reason: reason, err: err}
	}
	return err
}

// Enqueue validates req (an invalid one is a [*UsageError]) and publishes its ticket. The caller
// must then [Waiter.Poll] at least every 2 s, or [Waiter.Wait]: a Waiter not polled for 10 s
// becomes STALLED for the other clients and stops blocking them. Under a state directory format
// this client does not implement, the waiter waits in token-only mode and its grant is
// [Grant.TokenOnly].
//
// Enqueue first waits for the admission section, retried from 10 ms up to 250 ms (PROTOCOL.md
// section 8), until ctx ends; a process stopped inside the section blocks it until that process
// is continued or killed. When ctx ends first nothing is published and the error matches
// ctx.Err(); if another client held the section, it is a [*SectionBusyError] naming the pid
// admit.lock records for that client. Once inside the section the enqueue completes; ctx does
// not bound the wait for the admission, which Wait's own context does. A ctx that has already
// ended ends every Enqueue at once with ctx.Err(), a pass-through's included.
//
// A process started inside another run's grant inherits that grant's context (PROTOCOL.md
// section 17), and its request never queues behind its own parent, which may hold the whole
// budget. Unless Options.Independent is set, and when the environment's CORESLOT_STATE_DIR (or
// the default) is this Client's state directory, Enqueue applies the rule of a nested
// `coreslot run` (section 18):
//
//   - CORESLOT_UNBUDGETED is set (rule 1): the Waiter is admitted at once, with every requested
//     resource unbudgeted with its Min.
//   - CORESLOT_TOKEN_FDS is set and its descriptors are the parent's locked tokens (rule 2): the
//     Waiter is admitted at once, with min(Max, held) of each requested resource the parent
//     holds; a Min above what the parent holds is clamped to all of it (Clamped), and a
//     resource the parent holds none of is unbudgeted with its Min.
//   - A context that does not validate (a launcher closed the descriptors, or the parent is
//     gone) is an error matching ErrRefused, at once and with the diagnostic; nothing is
//     queued, rather than queue behind a parent that may hold the whole budget. It is the CLI's
//     refusal of section 14, and Options.Independent is the way to queue anyway.
//
// Such a grant reports [Grant.PassThrough]. It takes no token, publishes no ticket or hold, enters
// no section and bumps no gen: it shares its parent's grant, so pass-throughs are not budgeted
// against each other, and a caller that starts several commands at once partitions Granted itself.
// A Client opened on another state directory (Options.StateDir) is another budget, which the
// inherited context is not part of: its requests are ordinary. When the directory the environment
// names cannot be resolved (CORESLOT_STATE_DIR is not an absolute path, or it is unset and the home
// directory cannot be learned), a Client opened on Options.StateDir cannot tell whether it is that
// directory, and the request is refused (ErrRefused) rather than queued behind a parent that may
// hold the whole budget. A Client opened without Options.StateDir is on that directory by
// construction and never resolves it again. Resolving it may look the home directory up as Open
// does, and ctx does not bound that lookup's wait for getent.
func (c *Client) Enqueue(ctx context.Context, req Request) (*Waiter, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err // before a pass-through too, which waits for nothing
	}
	// Opened without Options.StateDir, the Client's directory is the one the environment names:
	// Open resolved it by the same rule, from the environment the context came in, and resolving
	// it again could run getent again and fail where Open's lookup succeeded.
	decide := engine.DecideNested
	if c.named {
		decide = engine.DecideNestedNamed
	}
	nested, err := decide(environ(), c.independent, c.e.Store().Dir())
	if err != nil {
		return nil, fmt.Errorf("%w: the inherited grant context cannot be used: %w", ErrRefused, err)
	}
	sreq := req.spec()
	if nested.Kind != engine.NotNested {
		return &Waiter{grant: &Grant{g: nested.PassThrough(sreq)}}, nil
	}
	t, err := c.e.EnqueueContext(ctx, sreq)
	if err != nil {
		return nil, wrap(err)
	}
	return &Waiter{t: t}, nil
}

// environ is this process's environment as a map; a later duplicate wins, as for exec. It is
// only read: the library never changes the environment its caller owns.
func environ() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// Adopt admits already-running work without queueing (PROTOCOL.md section 12.4), taking what
// is free up to each resource's Max. It has no minimum: the grant's Shortfall says what the work
// runs without budget ([Grant.Shortfall]). Adopt waits for the admission section until ctx
// ends, as Enqueue does: nothing is taken or published then.
//
// Adopt never passes through (PROTOCOL.md section 18 applies to Enqueue): inside another run's
// grant it takes what is free like any adoption, and the parent's tokens are not free.
func (c *Client) Adopt(ctx context.Context, req Request) (*Grant, error) {
	g, err := c.e.AdoptContext(ctx, req.spec())
	if err != nil {
		return nil, wrap(err)
	}
	return &Grant{g: g}, nil
}

// Available returns resource's size minus its locked tokens now, max(0, size - locked): what an
// admission could take of it at this moment, never below 0 (tokens locked at an index at or above a
// size that shrank count against it). It never waits for the section. A resource the configuration
// does not declare matches ErrRefused.
func (c *Client) Available(resource string) (int, error) {
	n, err := c.e.Free(resource)
	return n, wrap(err)
}

// Snapshot reads the state directory without writing anything further: the data behind
// `status --json`, as a [Report]. Open has already created the directory, res/ and admit.lock
// when they were missing; the coreslot command reads through a read-only open instead, which
// creates nothing.
func (c *Client) Snapshot() (Report, error) {
	r, err := c.e.Snapshot()
	if err != nil {
		return Report{}, wrap(err)
	}
	return newReport(r), nil
}

// Waiter is a queued request. Its methods are safe for concurrent use: the first Poll or Wait
// to admit wins and every other returns the same *Grant; Cancel after the admission returns
// [ErrAlreadyAdmitted]; Cancel during Wait makes it return [ErrCanceled]. A pass-through's Waiter
// ([Client.Enqueue]) is admitted from the start: Poll and Wait return its grant at once, and
// Cancel returns ErrAlreadyAdmitted. [Client.Enqueue] creates one: the zero Waiter is not
// usable.
type Waiter struct {
	t *engine.Ticket // nil for a pass-through

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

// Poll makes one non-blocking attempt (PROTOCOL.md section 8); call it at least every 2 s. It
// refreshes the ticket, checks for a cancel request and, when this waiter is the head, tries
// admission with a single try at the section. It returns the grant once admitted, and nil and
// nil while the request waits. Once the request is canceled (a cancel request, Cancel, or a Wait
// that ended without a grant) it returns ErrCanceled, now and on every later Poll. After a
// [*StateError] the request is still queued: poll again, or Cancel to withdraw it.
func (w *Waiter) Poll() (*Grant, error) {
	if w.t == nil {
		return w.grant, nil
	}
	r, err := w.t.Poll()
	if err != nil {
		return nil, wrap(err)
	}
	if a, ok := r.(engine.Admitted); ok {
		return w.wrapGrant(a.Grant), nil
	}
	return nil, nil
}

// Wait polls until admitted. When ctx ends first it withdraws the ticket (it is dead at once
// to every other client) and returns ctx.Err(); a Cancel makes it return ErrCanceled. Any other
// error (a *StateError, for example) also withdraws the ticket before it is returned.
func (w *Waiter) Wait(ctx context.Context) (*Grant, error) {
	if w.t == nil {
		return w.grant, nil
	}
	r, err := stepWhenDue(ctx, w.t)
	if a, ok := r.(engine.Admitted); ok && err == nil {
		return w.wrapGrant(a.Grant), nil
	}
	if err == nil || ctx.Err() != nil {
		return w.abandon(ctx)
	}
	if errors.Is(err, engine.ErrCanceled) {
		return nil, err
	}
	// Any other error ends the wait: withdraw, so that the ticket is dead at once instead of
	// blocking the queue unrefreshed until it stalls. An admission that won the race is
	// returned instead.
	if errors.Is(w.t.Withdraw(), engine.ErrAlreadyAdmitted) {
		if g, perr := w.Poll(); g != nil && perr == nil {
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
		if g, perr := w.Poll(); g != nil && perr == nil {
			return g, nil
		}
	}
	return nil, ctx.Err()
}

// Cancel withdraws the request by closing its ticket descriptor; it never waits for the
// section. It is idempotent before admission and returns ErrAlreadyAdmitted after it.
func (w *Waiter) Cancel() error {
	if w.t == nil {
		return ErrAlreadyAdmitted
	}
	return wrap(w.t.Withdraw())
}

// Grant is what an admission holds. Its methods are safe for concurrent use. [Waiter.Wait],
// [Waiter.Poll] and [Client.Adopt] create one: the zero Grant is not usable.
type Grant struct {
	g grant
}

// grant is what Grant wraps: an admission's, an adoption's or a fail-open's *engine.Grant, or a
// pass-through's *engine.PassThrough.
type grant interface {
	Grant() map[string]int
	Clamped() map[string]int
	Shortfall() map[string]int
	Unbudgeted() []string
	TokenOnly() bool
	Files() []*os.File
	Env() []string
	Close() error
}

// Granted maps every requested resource to what the command may use, as CORESLOT_GRANTED_<RES>
// tells it: the tokens held (for a pass-through, min(Max, held) of the parent's), or for a
// resource run unbudgeted, its requested Min.
func (g *Grant) Granted() map[string]int { return g.g.Grant() }

// Clamped maps each resource whose Min exceeded its whole size to that Min; such a grant holds
// the whole resource. For a pass-through it maps each resource whose Min exceeded what the
// parent holds to that Min, and the grant is all the parent holds of it. Clamped records the
// requested Min; [Grant.Shortfall] says how far short of it the grant is.
func (g *Grant) Clamped() map[string]int { return g.g.Clamped() }

// Shortfall maps each requested resource the grant holds fewer than Min of to Min minus what it
// holds: what the work runs without budget. An adoption (Client.Adopt) takes what is free, so it
// may fall short of any Min; an admission or a pass-through falls short only where Clamped
// says so. A resource run unbudgeted (Unbudgeted) is never in it. It is empty when the grant
// holds every Min.
func (g *Grant) Shortfall() map[string]int { return g.g.Shortfall() }

// Unbudgeted lists, sorted, the requested resources run unbudgeted: those the configuration does
// not declare; for a pass-through, those the parent holds none of, or every one under a parent
// that runs unbudgeted.
func (g *Grant) Unbudgeted() []string { return g.g.Unbudgeted() }

// TokenOnly reports a token-only grant (an unknown state directory format): no hold, and Env
// carries CORESLOT_UNBUDGETED=format.
func (g *Grant) TokenOnly() bool { return g.g.TokenOnly() }

// PassThrough reports a pass-through's grant ([Client.Enqueue]): the parent's, shared with every
// other pass-through under the same parent rather than budgeted against them.
func (g *Grant) PassThrough() bool {
	_, ok := g.g.(*engine.PassThrough)
	return ok
}

// Files returns the descriptors for exec.Cmd.ExtraFiles: the hold as fd 3 (nil when there is
// none, so fd 3 stays closed), then the tokens from fd 4. A pass-through's are the parent's hold
// and every token the parent passed on, which ExtraFiles places at those numbers whatever
// numbers this process inherited them at; under a parent that runs unbudgeted there are none,
// and the inherited descriptors stay where they are.
//
// The Grant owns the files: do not close them. [Grant.Close] closes this process's copies, and a
// pass-through's are the ones this process inherited, which nothing in the library closes.
func (g *Grant) Files() []*os.File { return g.g.Files() }

// Env returns the CORESLOT_* environment matching Files. [Grant.Environ] builds a command's
// environment from it; a caller that builds its own removes every inherited CORESLOT_GRANTED_*,
// CORESLOT_HELD_FD, CORESLOT_TOKEN_FDS and CORESLOT_UNBUDGETED before appending Env (PROTOCOL.md
// section 17): inherited grant context describes another grant. CORESLOT_STATE_DIR is not grant
// context and is kept. A pass-through's Env carries the inherited context on, as a nested
// `coreslot run` passes it to its command: CORESLOT_GRANTED_<RES> narrowed for each requested
// resource, the descriptors' new numbers, and every other inherited context variable unchanged.
func (g *Grant) Env() []string { return g.g.Env() }

// Environ returns the environment of a command started with Files as its exec.Cmd.ExtraFiles:
// base (typically os.Environ()) without its grant context, the variables CORESLOT_GRANTED_*,
// CORESLOT_HELD_FD, CORESLOT_TOKEN_FDS and CORESLOT_UNBUDGETED (PROTOCOL.md section 17),
// followed by Env. It does not modify base.
//
//	cmd.ExtraFiles, cmd.Env = g.Files(), g.Environ(os.Environ())
func (g *Grant) Environ(base []string) []string {
	own := g.g.Env()
	env := make([]string, 0, len(base)+len(own))
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !engine.IsContext(k) {
			env = append(env, kv)
		}
	}
	return append(env, own...)
}

// Close closes this process's copies of the descriptors (never unlocking, so a child keeps the
// grant until it exits) and removes the hold when no child holds it, waiting at most 2 s for
// the section. It is idempotent. When the hold or a token is no longer the file at its path (the
// state directory was removed, or removed and recreated, while the grant was held) it closes the
// descriptors, writes nothing into the directory as it now exists, and returns a [*StateError].
// A pass-through's Close closes nothing: its descriptors are the ones this process inherited,
// which keep the parent's grant held and serve every later pass-through of the process.
func (g *Grant) Close() error { return wrap(g.g.Close()) }
