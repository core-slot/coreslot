//go:build unix

package main

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/store"
)

// failOpen handles a state error (PROTOCOL.md section 14): under --strict a refusal, otherwise
// the command runs at once with min of each resource, a diagnostic, an unbudgeted hold, the
// degraded record and the run log when the directory is usable and its format understood, and
// CORESLOT_UNBUDGETED. e is nil when the directory could not be opened.
func (r *run) failOpen(e *engine.Engine, err error) int {
	reason := err.Error()
	var se *stateError
	if errors.As(err, &se) {
		reason = se.Reason
	}
	if r.o.strict {
		code, _ := classify(strictRefusal(err, r.o.refusalCode))
		r.notice("state error: %s; refusing (--strict)", reason)
		return code
	}
	r.notice("state error: %s; running unbudgeted with %s (fail-open)", reason,
		grantText(r.o.minGrant(), r.o.sortedNeed()))
	c := child{grant: r.o.minGrant(), unbudgeted: r.o.sortedNeed(), failOpen: reason, release: func() error { return nil }}
	var g *engine.Grant
	var ferr error
	if e != nil {
		ferr = inSection(func() (err error) {
			g, err = e.FailOpen(r.o.request(0), reason)
			return err
		})
	}
	if g != nil {
		c.env, c.files, c.release = append(slices.Clone(r.env), g.Env()...), g.Files(), func() error { return inSection(g.Close) }
		if g.HoldName() != "" {
			c.log = e.Store() // FORMAT was understood in the section that published the hold
		}
	} else {
		c.env = append(slices.Clone(r.env), unbudgetedEnv(r.o.minGrant(), reason)...)
	}
	if msg := failOpenNotice(e != nil, g != nil && g.HoldName() != "", ferr); msg != "" {
		r.notice("%s", msg)
	}
	return r.runChild(c)
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
		return fmt.Sprintf("the unbudgeted hold, the degraded record and the run log were not written: %v", err)
	case err != nil:
		return fmt.Sprintf("the degraded record was not written: %v", err)
	}
	return ""
}

// unbudgetedEnv is the environment of a run without a grant: each resource's min, and the
// reason when there is one.
func unbudgetedEnv(grant map[string]int, reason string) []string {
	var env []string
	for _, name := range slices.Sorted(maps.Keys(grant)) {
		env = append(env, "CORESLOT_GRANTED_"+spec.EnvName(name)+"="+strconv.Itoa(grant[name]))
	}
	if reason != "" {
		env = append(env, "CORESLOT_UNBUDGETED="+reason)
	}
	return env
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
	c := child{grant: r.o.minGrant(), unbudgeted: r.o.sortedNeed(), failOpen: timeoutReason, release: func() error { return nil }}
	var g *engine.Grant
	herr := inSection(func() (err error) {
		g, err = e.UnbudgetedHold(r.o.request(0), timeoutReason)
		return err
	})
	if g != nil {
		c.env, c.files, c.release = append(slices.Clone(r.env), g.Env()...), g.Files(), func() error { return inSection(g.Close) }
	} else {
		c.env = append(slices.Clone(r.env), unbudgetedEnv(r.o.minGrant(), timeoutReason)...)
	}
	if timeoutLogs(g != nil && g.HoldName() != "", herr, t != nil && !t.TokenOnly()) {
		c.log = e.Store()
	}
	if herr != nil {
		r.notice("the unbudgeted hold was not published: %v", herr)
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
