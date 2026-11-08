//go:build unix

package main

import (
	"errors"
	"fmt"
	"slices"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/telemetry"
)

// failOpen handles a state error (PROTOCOL.md section 14): under --strict a refusal, otherwise
// the command runs at once with its need, a diagnostic, an unbudgeted hold, the
// degraded record and the run log when the directory is usable and its format understood, and
// CORESLOT_UNBUDGETED. e is nil when the directory could not be opened.
func (r *run) failOpen(e *engine.Engine, err error) int {
	var format *store.FormatError
	// A step's malformed FORMAT is a plain state error, so the recorder reads it for itself.
	if errors.As(err, &format) || e != nil && !r.rec.Stopped() && formatStillUnderstood(e.Store()) != nil {
		r.rec.Stop()
	}
	if format != nil && format.Err == nil {
		// A foreign format: the one notice says what to do, and nothing can be written there.
		if r.o.strict {
			code, _ := classify(strictRefusal(err, r.o.refusalCode))
			r.notice("%s; refusing (--strict)", format.Error())
			return code
		}
		r.notice("%s; running unbudgeted with mem=%s", format.Error(), spec.FormatMiB(r.o.need))
		return r.runChild(r.unbudgetedChild(format.Error(), "", nil))
	}
	reason, ok := stateReason(err)
	if !ok {
		reason = err.Error()
	}
	if r.o.strict {
		code, _ := classify(strictRefusal(err, r.o.refusalCode))
		r.notice("state error: %s; refusing (--strict)", text(reason))
		r.recordWaitEnd("refused", telemetry.Fields{"reason": "strict"})
		return code
	}
	r.notice("state error: %s; running unbudgeted with mem=%s (fail-open)", text(reason), spec.FormatMiB(r.o.need))
	var g *engine.Grant
	var ferr error
	if e != nil {
		g, ferr = e.FailOpen(r.o.request(), reason)
	}
	c := r.unbudgetedChild(reason, "state_error", g)
	if g != nil && g.HoldName() != "" {
		c.log = e.Store() // FORMAT was understood in the section that published the hold
		r.recordOutside(true, "fail_open", "state_error", "")
	} else {
		r.rec.Stop()
	}
	if msg := failOpenNotice(e != nil, g != nil && g.HoldName() != "", ferr); msg != "" {
		r.notice("%s", msg)
	}
	return r.runChild(c)
}

// unbudgetedChild is the child of a run outside the budget for reason (code is its closed name
// on the telemetry line), with its need: g's
// environment and descriptor when its unbudgeted hold g was published (g may be nil), else the
// unbudgeted environment engine.ChildEnv builds. The caller decides whether it logs.
func (r *run) unbudgetedChild(reason, code string, g *engine.Grant) child {
	c := child{grantMiB: r.o.need, failOpen: reason, failCode: code, release: func() error { return nil }}
	if g != nil {
		c.env, c.files, c.release = append(slices.Clone(r.env), g.Env()...), g.Files(), g.Close
	} else {
		c.env = append(slices.Clone(r.env), engine.ChildEnv(r.o.need, "", reason)...)
	}
	return c
}

// recordOutside writes the line of a run with no ticket, or whose ticket is gone: kind and the
// closed reason, with the parent's hold when it has one. understood says this run read FORMAT and
// understood it, in its hold's section or as a pass-through: only then may it make a recorder.
func (r *run) recordOutside(understood bool, kind, reason, parent string) {
	if !r.telemetry {
		return
	}
	if r.rec == nil && understood {
		r.rec = r.newRecorder()
	}
	f := telemetry.Fields{"kind": kind, "reason": reason, "need_mib": r.o.need}
	if parent != "" {
		f["parent"] = parent
	}
	r.rec.Emit("outside", f)
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

// timeoutRun is --on-timeout run: the ticket is withdrawn and the command runs with its need
// outside the budget. Its unbudgeted hold shows it in status and its run-log line gives
// the reason, but a timeout is not a state error: nothing is recorded as degraded.
func (r *run) timeoutRun(e *engine.Engine) int {
	r.notice("not admitted within %v (--timeout); running without the budget with mem=%s (--on-timeout run)",
		r.o.timeout, spec.FormatMiB(r.o.need))
	g, herr := e.UnbudgetedHold(r.o.request(), timeoutReason)
	c := r.unbudgetedChild(timeoutReason, "timeout", g)
	published := g != nil && g.HoldName() != ""
	if timeoutLogs(published, herr) {
		c.log = e.Store()
		r.recordOutside(published, "timeout_run", "timeout", "")
	} else {
		r.rec.Stop()
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
func timeoutLogs(holdPublished bool, holdErr error) bool {
	var se *store.StateError
	switch {
	case holdPublished:
		return true
	case errors.Is(holdErr, engine.ErrRefused), errors.As(holdErr, &se):
		return false
	}
	return true
}
