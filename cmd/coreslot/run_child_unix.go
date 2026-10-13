//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/testhooks"
)

// admitted runs the command on an admitted grant.
func (r *run) admitted(e *engine.Engine, g *engine.Grant) int {
	for _, name := range slices.Sorted(maps.Keys(g.Clamped())) {
		n := g.Grant()[name]
		r.notice("%s: requested %d, budget is %d; running alone with %d", name, g.Clamped()[name], n, n)
	}
	if g.TokenOnly() {
		r.notice("the state directory's format is not implemented here; running in token-only mode, outside the queue")
	}
	if un := g.Unbudgeted(); len(un) > 0 && !g.TokenOnly() {
		r.notice("%s: not declared in the configuration; running unbudgeted with %s",
			strings.Join(un, ", "), grantText(g.Grant(), un))
	}
	c := child{
		env:        append(slices.Clone(r.env), g.Env()...),
		files:      g.Files(),
		grant:      g.Grant(),
		clamped:    slices.Sorted(maps.Keys(g.Clamped())),
		unbudgeted: g.Unbudgeted(),
		release:    g.Close,
	}
	if !g.TokenOnly() {
		c.log = e.Store() // FORMAT was understood in the admitting section
	}
	return r.runChild(c)
}

func grantText(grant map[string]int, names []string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, grant[name]))
	}
	return strings.Join(parts, ", ")
}

// child is what a run starts its command with.
type child struct {
	env        []string
	files      []*os.File // ExtraFiles: fd 3 (nil without a hold), then the tokens
	grant      map[string]int
	clamped    []string
	unbudgeted []string
	failOpen   string
	nested     bool
	release    func() error // closes the run's copies of the grant once the command exited
	log        *store.Store // the run log to append to; nil writes none
}

// runChild starts the command with c, waits for it, releases the grant and appends the run
// log line. It returns the command's status, or the status of a command that could not start.
// A release that finds the grant's files replaced (the state directory was removed or recreated
// under the run, I1) has written nothing into the directory: the run says so and appends no
// run-log line, and still returns the command's status.
func (r *run) runChild(c child) int {
	argv := substitute(r.o.argv, c.grant, r.o.subst)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.io.Stdin, r.io.Stdout, r.io.Stderr
	cmd.Env = c.env
	if slices.ContainsFunc(c.files, func(f *os.File) bool { return f != nil }) {
		cmd.ExtraFiles = c.files
	}
	inherited := 0
	if cmd.ExtraFiles != nil {
		inherited = 3 + len(cmd.ExtraFiles)
	}
	_, _ = raiseChildNofile(r.nofile.originalNofile(), inherited)
	if sig := pendingSignal(); sig != 0 {
		// Interrupted between the admission and the start: the command never runs.
		_ = c.release()
		code, _ := classify(&interruptError{Sig: sig})
		return code
	}
	terminal := terminalMode(r.io.Stdin)
	prepareChild(cmd, terminal)
	started := time.Now()
	status := 0
	var released error
	if err := cmd.Start(); err != nil {
		childDone(terminal)
		released = c.release()
		code, _ := classify(&execError{Err: err})
		r.notice("cannot run %s: %s", text(argv[0]), text(err.Error()))
		status = code
	} else {
		status = superviseChild(cmd, terminal)
		childDone(terminal)
		released = c.release()
	}
	if r.releaseFailed(released) {
		return status
	}
	if c.log != nil || c.nested {
		rec := store.RunRecord{
			TimeNS: r.startNS, Label: r.o.label, Need: r.o.need, Grant: c.grant, Clamped: c.clamped,
			UnbudgetedResources: c.unbudgeted, WaitedS: r.waited.Seconds(),
			RanS: time.Since(started).Seconds(), Exit: status, Load1: r.load, FailOpen: c.failOpen,
			Nested: c.nested,
		}
		recordUsage(&rec, cmd.ProcessState) // nil when the command never started
		var err error
		if c.nested {
			err = appendNestedLog(rec) // reads FORMAT outside the section only now, after the command
		} else {
			err = appendRunLog(c.log, rec, r.notice)
		}
		if err != nil {
			r.notice("the run log was not written: %s", text(err.Error()))
		}
	}
	return status
}

// recordUsage copies the command's CPU time and peak resident set size from its wait4 rusage
// into rec (PROTOCOL.md section 20). The rusage covers the command and the descendants it waited
// for, not the ones it left running. A command that never started has no ProcessState and gets
// none of the keys, as does a platform whose wait reports no rusage: an unknown value is left out
// rather than written as zero. The seconds are not rounded, like ran_s and waited_s.
func recordUsage(rec *store.RunRecord, ps *os.ProcessState) {
	if ps == nil {
		return
	}
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return
	}
	user := time.Duration(ru.Utime.Nano()).Seconds()
	sys := time.Duration(ru.Stime.Nano()).Seconds()
	rec.CPUUserS, rec.CPUSysS = &user, &sys
	if kib, known := maxRSSKiB(int64(ru.Maxrss)); known {
		rec.MaxRSSKiB = &kib
	}
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
// missing is true only when FORMAT does not exist (ENOENT), which a pass-through does not report.
func formatStillUnderstood(dir string) (missing bool, err error) {
	n, present, err := store.ReadFormatAt(dir)
	switch {
	case err != nil:
		return false, err
	case !present:
		return true, errFormatChanged
	case !slices.Contains(testhooks.Formats(), n):
		return false, errFormatChanged
	}
	return false, nil
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
// and the line is appended. A busy section only skips the rotation. A pass-through never rotates
// (appendNestedLog).
func appendRunLog(st *store.Store, rec store.RunRecord, notice func(format string, args ...any)) error {
	if st.RunLogNeedsRotation() {
		rotateErr, skip := rotateRunLog(st)
		if skip != nil {
			return skip
		}
		if rotateErr != nil {
			notice("the run log was not rotated: %s", text(rotateErr.Error()))
		}
	}
	if _, err := formatStillUnderstood(st.Dir()); err != nil {
		return err
	}
	return st.AppendRun(rec)
}

// rotateRunLog rotates the run log inside the section, if it can enter within 2 s. skip is the
// error that stops the append (a FORMAT not understood, or a malformed one); rotateErr is any
// other failure, which costs the rotation alone. A busy section returns neither.
func rotateRunLog(st *store.Store) (rotateErr, skip error) {
	ctx, cancel := context.WithTimeout(context.Background(), logRotateWait)
	defer cancel()
	sec, err := st.Enter(ctx)
	if err != nil {
		return nil, nil // busy: append without rotating
	}
	defer sec.Leave()
	_, understood, err := sec.Format(testhooks.Formats())
	if !understood {
		if err != nil {
			return nil, err // malformed FORMAT
		}
		return nil, errFormatChanged
	}
	if err == nil {
		// A section entered to rotate reconciles first when it is due, like every entry other
		// than admission (PROTOCOL.md section 10.4).
		_, err = sec.ReconcileIfDue(store.NowNS())
	}
	if err == nil {
		err = sec.RotateLogIfNeeded()
	}
	return err, nil
}
