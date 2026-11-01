//go:build unix

package engine

import (
	"context"
	"errors"

	"github.com/core-slot/coreslot/internal/spec"
)

// FailOpen publishes the hold of a run its caller has decided to run unbudgeted (PROTOCOL.md
// section 14): a need of 0, which reserves nothing, and the reason recorded in degraded, in one
// section entered within 2 s. The engine never makes that decision itself. reason is
// informational, non-empty text.
//
// The returned grant is always usable: its Env carries CORESLOT_UNBUDGETED=reason and the
// request's need. When nothing could be written (the section stayed busy, a state error, or a
// FORMAT this client does not implement or that is malformed) it has no hold, and err says why;
// the caller still runs and reports it. Close removes the hold as after any run (section 12.3).
func (e *Engine) FailOpen(req spec.Request, reason string) (*Grant, error) {
	return e.unbudgetedHold(req, reason, true)
}

// UnbudgetedHold is FailOpen for a run outside the budget that is not a state error, such as a
// wait that timed out with --on-timeout run: the same unbudgeted hold, so that status shows the
// run, and CORESLOT_UNBUDGETED=reason, but no degraded record.
func (e *Engine) UnbudgetedHold(req spec.Request, reason string) (*Grant, error) {
	return e.unbudgetedHold(req, reason, false)
}

func (e *Engine) unbudgetedHold(req spec.Request, reason string, degraded bool) (*Grant, error) {
	if reason == "" {
		return nil, errors.New("engine: an unbudgeted run needs a reason")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	g := &Grant{e: e, grantMiB: req.NeedMiB, failOpen: reason}
	ctx, cancel := context.WithTimeout(context.Background(), releaseWait)
	defer cancel()
	sec, err := enterSection(e.st, ctx)
	if err != nil {
		return g, err
	}
	defer sec.Leave()
	n, understood, err := sec.Prepare(e.clock.Realtime())
	if err != nil {
		return g, err
	}
	if !understood {
		return g, refuseFormat(n, "; nothing written")
	}
	if err := g.publishHold(sec, req, e.clock.Realtime(), 0); err != nil {
		return g, err
	}
	_ = sec.BumpGen()
	if !degraded {
		return g, nil
	}
	return g, sec.RecordDegraded(reason)
}
