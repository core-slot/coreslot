//go:build unix

package main

import (
	"errors"
	"fmt"
	"slices"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/store"
)

// failOpen handles a state error (PROTOCOL.md section 14): under --strict a refusal, otherwise
// the command runs at once with min of each resource, a diagnostic, an unbudgeted hold, the
// degraded record and the run log when the directory is usable and its format understood, and
// CORESLOT_UNBUDGETED. e is nil when the directory could not be opened.
func (r *run) failOpen(e *engine.Engine, err error) int {
	reason, ok := stateReason(err)
	if !ok {
		reason = err.Error()
	}
	if r.o.strict {
		code, _ := classify(strictRefusal(err, r.o.refusalCode))
		r.notice("state error: %s; refusing (--strict)", text(reason))
		return code
	}
	r.notice("state error: %s; running unbudgeted with %s (fail-open)", text(reason),
		grantText(r.o.minGrant(), r.o.sortedNeed()))
	var g *engine.Grant
	var ferr error
	if e != nil {
		g, ferr = e.FailOpen(r.o.request(0), reason)
	}
	c := r.unbudgetedChild(reason, g)
	if g != nil && g.HoldName() != "" {
		c.log = e.Store() // FORMAT was understood in the section that published the hold
	}
	if msg := failOpenNotice(e != nil, g != nil && g.HoldName() != "", ferr); msg != "" {
		r.notice("%s", msg)
	}
	return r.runChild(c)
}

// unbudgetedChild is the child of a run outside the budget for reason, with min of each
// resource: g's environment and descriptors when its unbudgeted hold g was published (g may be
// nil), else the unbudgeted environment engine.ChildEnv builds. The caller decides whether it logs.
func (r *run) unbudgetedChild(reason string, g *engine.Grant) child {
	c := child{grant: r.o.minGrant(), unbudgeted: r.o.sortedNeed(), failOpen: reason, release: func() error { return nil }}
	if g != nil {
		c.env, c.files, c.release = append(slices.Clone(r.env), g.Env()...), g.Files(), g.Close
	} else {
		c.env = append(slices.Clone(r.env), engine.ChildEnv(r.o.minGrant(), false, 0, reason)...)
	}
	return c
}

// strictRefusal is what --strict makes of a state error: a refusal with the run's refusal code.
func strictRefusal(err error, code int) error {
	return &refusalError{Msg: "state error under --strict: " + err.Error(), Code: code}
}

// failOpenNotice says what a fail-open run could not record (PROTOCOL.md section 14): opened
// reports a usable state directory, held a published unbudgeted hold, and err what failed. The
// degraded record is written after the hold, and the run log only with a hold, so a missing hold
// means none of the three; a hold with an error means the degraded record alone failed.
func failOpenNotice(opened, held bool, err error) string {
	switch {
	case !opened:
		return "nothing was written (hold, degraded record, run log): the state directory cannot be used"
	case !held:
		return fmt.Sprintf("the unbudgeted hold, the degraded record and the run log were not written: %s", text(fmt.Sprint(err)))
	case err != nil:
		return fmt.Sprintf("the degraded record was not written: %s", text(err.Error()))
	}
	return ""
}

// timeoutReason is the reason a timeout run gives in its hold, its run-log line and
// CORESLOT_UNBUDGETED.
const timeoutReason = "timeout"

// timeoutRun is --on-timeout run: the ticket is withdrawn and the command runs with min of each
// resource outside the budget. Its unbudgeted hold shows it in status and its run-log line gives
// the reason, but a timeout is not a state error: nothing is recorded as degraded.
func (r *run) timeoutRun(e *engine.Engine, t *engine.Ticket) int {
	r.notice("not admitted within %v (--timeout); running without the budget with %s (--on-timeout run)",
		r.o.timeout, grantText(r.o.minGrant(), r.o.sortedNeed()))
	g, herr := e.UnbudgetedHold(r.o.request(0), timeoutReason)
	c := r.unbudgetedChild(timeoutReason, g)
	if timeoutLogs(g != nil && g.HoldName() != "", herr, t != nil && !t.TokenOnly()) {
		c.log = e.Store()
	}
	if herr != nil {
		r.notice("the unbudgeted hold was not published: %s", text(herr.Error()))
	}
	return r.runChild(c)
}

// timeoutLogs decides whether a timeout run appends its run-log line (PROTOCOL.md section 14):
// when its hold was published, FORMAT was understood in that section; when the hold's section
// found a FORMAT it does not understand (ErrRefused) or a malformed one (a state error), it
// writes nothing; otherwise (the section stayed busy) the FORMAT understood when its ticket was
// published still counts.
func timeoutLogs(holdPublished bool, holdErr error, ticketUnderstood bool) bool {
	var se *store.StateError
	switch {
	case holdPublished:
		return true
	case errors.Is(holdErr, engine.ErrRefused), errors.As(holdErr, &se):
		return false
	}
	return ticketUnderstood
}

// lightReason is the reason a --light run gives in its hold, its run-log line and
// CORESLOT_UNBUDGETED.
const lightReason = "light"

// lightRun is --light: the command runs at once with min of each resource outside the budget,
// as a timeout run does but without having queued. Its unbudgeted hold shows it in status, and
// its run-log line keeps the cores it used, which the usage hint compares with one core.
func (r *run) lightRun(e *engine.Engine) int {
	g, herr := e.UnbudgetedHold(r.o.request(0), lightReason)
	c := r.unbudgetedChild(lightReason, g)
	if timeoutLogs(g != nil && g.HoldName() != "", herr, false) {
		c.log = e.Store()
	}
	if herr != nil {
		r.notice("the unbudgeted hold was not published: %s", text(herr.Error()))
	}
	return r.runChild(c)
}
