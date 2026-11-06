//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// memSampleEvery is the period of the command tree's memory sampler.
const memSampleEvery = time.Second

// admitted runs the command on an admitted grant.
func (r *run) admitted(e *engine.Engine, g *engine.Grant) int {
	c := child{
		env:      append(slices.Clone(r.env), g.Env()...),
		files:    g.Files(),
		grantMiB: g.GrantMiB(),
		release:  g.Close,
		log:      e.Store(), // FORMAT was understood in the admitting section
		grant:    g,
	}
	return r.runChild(c)
}

// child is what a run starts its command with.
type child struct {
	env      []string
	files    []*os.File // ExtraFiles: fd 3, nil without a hold
	grantMiB int64
	failOpen string
	release  func() error  // closes the run's copies of the grant once the command exited
	log      *store.Store  // the run log to append to; nil writes none
	grant    *engine.Grant // the admitted grant the watchdog ticks for; nil outside the budget
}

// runChild starts the command with c, waits for it, releases the grant and appends the run
// log line. It returns the command's status, or the status of a command that could not start.
// A release that finds the grant's files replaced (the state directory was removed or recreated
// under the run, I1) has written nothing into the directory: the run says so and appends no
// run-log line, and still returns the command's status.
func (r *run) runChild(c child) int {
	argv := r.o.argv
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.io.Stdin, r.io.Stdout, r.io.Stderr
	cmd.Env = c.env
	if slices.ContainsFunc(c.files, func(f *os.File) bool { return f != nil }) {
		cmd.ExtraFiles = c.files
	}
	if sig := pendingSignal(); sig != 0 {
		// Interrupted between the admission and the start: the command never runs.
		_ = c.release()
		code, _ := classify(&interruptError{Sig: sig})
		return code
	}
	// --same-group takes the terminal-mode branch: the command stays in coreslot's group.
	terminal := r.o.sameGroup || terminalMode(r.io.Stdin)
	prepareChild(cmd, terminal)
	if !r.o.noRestart && r.o.restart != "no" { // a command that must not be restarted is not put first in line
		raiseOOMScore()
	}
	started := time.Now()
	r.closed = watch.Job{}
	status := 0
	var released error
	var sup *supervisor
	var p peak
	stopped := false
	if err := exposeInherited(cmd.Start); err != nil {
		childDone(terminal)
		released = c.release()
		code, _ := classify(&execError{Err: err})
		r.notice("cannot run %s: %s", text(argv[0]), text(err.Error()))
		status = code
	} else {
		sup = newSupervisor(cmd, !terminal)
		ctx, stop := context.WithCancel(context.Background())
		watched := make(chan struct{})
		go func() {
			defer close(watched)
			stopped, p = r.watchChild(ctx, sup, c.grant, c.log)
		}()
		status = superviseChild(cmd, sup, terminal)
		stop()
		<-watched
		if c.grant != nil {
			r.updateHold(c.log, c.grant, func(h *wire.Hold) { h.ChildExited = true })
		}
		childDone(terminal)
		released = c.release()
	}
	killed := sup != nil && sup.ws.Signaled() && sup.ws.Signal() == syscall.SIGKILL // not a command's own exit 137
	r.stopped, r.peakKiB = stopped && killed, p.kib
	if c.grant != nil && killed && !r.stopped {
		r.notice("ended by SIGKILL from outside coreslot; not restarted")
	}
	if r.releaseFailed(released) {
		return status
	}
	rec := store.RunRecord{
		TimeNS: r.start.UnixNano(), Label: r.o.label, WaitedS: r.waited.Seconds(),
		RanS: time.Since(started).Seconds(), Exit: status, FailOpen: c.failOpen,
		Container: p.container || r.o.containerCommand(),
	}
	if r.stopped {
		rec.Stops = r.stops + 1
	}
	var maxRSS *int64
	if sup != nil {
		maxRSS = recordUsage(&rec, sup.usage())
	}
	setMemPeak(&rec, maxRSS, p)
	rec.TickShare = r.tickShare()
	if c.log != nil {
		if err := appendRunLog(c.log, rec, r.notice); err != nil {
			r.notice("the run log was not written: %s", text(err.Error()))
		}
	}
	return status
}

// recordUsage copies the command's user and system seconds from its wait4 rusage into rec
// (PROTOCOL.md section 20) and returns its peak resident set size in KiB. The rusage covers the
// command and the descendants it waited for, not the ones it left running. A command that never
// started, or whose reap read no rusage (ru nil), gets no keys and a nil peak: an unknown value is
// left out rather than written as zero. The seconds are not rounded, like ran_s and waited_s.
func recordUsage(rec *store.RunRecord, ru *syscall.Rusage) (maxRSS *int64) {
	if ru == nil {
		return nil
	}
	user := time.Duration(ru.Utime.Nano()).Seconds()
	sys := time.Duration(ru.Stime.Nano()).Seconds()
	rec.UserS, rec.SysS = &user, &sys
	if kib, known := maxRSSKiB(int64(ru.Maxrss)); known {
		return &kib
	}
	return nil
}

// releaseFailed reports a release that returned err and says whether the run must stop writing.
// Only an identity mismatch (*engine.IdentityError, PROTOCOL.md section 12.3) stops it: the
// release wrote nothing into the state directory as it now exists, and neither does the run, not
// even its run-log line. A descriptor that failed to close is reported, and the run goes on.
func (r *run) releaseFailed(err error) (stop bool) {
	var mismatch *engine.IdentityError
	switch {
	case err == nil:
		return false
	case errors.As(err, &mismatch):
		reason, ok := stateReason(mismatch.Err)
		if !ok {
			reason = mismatch.Error()
		}
		r.notice("state error at release: %s; nothing more was written into the state directory, not even the run log", text(reason))
		return true
	}
	r.notice("the grant's descriptors did not all close: %s", text(err.Error()))
	return false
}

// errFormatChanged is the outcome of a run-log append that found a FORMAT this client does not
// understand, in the rotation section or when re-reading it just before the append: the line is
// not appended (PROTOCOL.md sections 14 and 20).
var errFormatChanged = errors.New("the state directory's FORMAT is missing or names a format this client does not implement")

// formatStillUnderstood re-reads FORMAT outside the section immediately before a run-log line is
// appended, since it may have changed while the command ran (PROTOCOL.md section 20). It returns
// errFormatChanged when FORMAT does not exist or names a format this client does not implement,
// and the error of a FORMAT that cannot be read (not a regular file, unreadable) or is malformed.
func formatStillUnderstood(st *store.Store) error {
	n, present, err := st.ReadFormat()
	switch {
	case err != nil:
		return err
	case !present || !store.Understood(n):
		return errFormatChanged
	}
	return nil
}

// appendRunLog appends rec to the run log outside the section (PROTOCOL.md section 20). When the
// log is at or above 10 MiB it first rotates it inside the section, if it can enter within 2 s,
// reconciling first when reconciliation is due (section 10.4). Then it re-reads FORMAT and
// appends only when it is still one this client implements.
// Only a FORMAT that is not understood, or a malformed one, in the rotation section, or at that
// re-read a FORMAT that is missing, unreadable (not a regular file, no permission), malformed or
// of another format, stops the append, which would otherwise write into a directory whose format
// it does not understand (section 14). A run's release section, which comes first, normally
// writes a FORMAT the command removed; FORMAT is still missing at the re-read when it was removed
// after that, when the release could not enter the section within 2 s, or when the run published
// no hold to release. Any other rotation failure costs the rotation, not the line: notice reports it
// and the line is appended. A section the rotation cannot enter (held for 2 s, or admit.lock
// unusable) only skips the rotation.
func appendRunLog(st *store.Store, rec store.RunRecord, notice func(format string, args ...any)) error {
	if st.RunLogNeedsRotation() {
		err := st.RotateLog()
		var notEntered *store.NotEnteredError
		var format *store.FormatError
		switch {
		case errors.As(err, &notEntered):
			// Append without rotating.
		case errors.As(err, &format) && format.Err != nil:
			return format.Err // FORMAT is malformed or cannot be read
		case errors.As(err, &format):
			return errFormatChanged
		case err != nil:
			notice("the run log was not rotated: %s", text(err.Error()))
		}
	}
	if err := formatStillUnderstood(st); err != nil {
		return err
	}
	return st.AppendRun(rec)
}
