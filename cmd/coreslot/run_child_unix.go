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
		release:    func() error { return inSection(g.Close) },
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
	_, _ = raiseChildNofile(currentNofile())
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
		code, _ := classify(err)
		r.notice("cannot run %s: %v", argv[0], err)
		status = code
	} else {
		status = superviseChild(cmd, terminal)
		childDone(terminal)
		released = c.release()
	}
	if released != nil {
		reason := released.Error()
		if rs, ok := engine.StateReason(released); ok {
			reason = rs
		}
		r.notice("state error at release: %s; nothing more was written into the state directory, not even the run log", reason)
		return status
	}
	if c.log != nil || c.nested {
		rec := store.RunRecord{
			TimeNS: r.startNS, Label: r.o.label, Need: r.o.need, Grant: c.grant, Clamped: c.clamped,
			UnbudgetedResources: c.unbudgeted, WaitedS: r.waited.Seconds(),
			RanS: time.Since(started).Seconds(), Exit: status, Load1: r.load, FailOpen: c.failOpen,
			Nested: c.nested,
		}
		var err error
		if c.nested {
			err = appendNestedLog(rec) // reads FORMAT outside the section only now, after the command
		} else {
			err = appendRunLog(c.log, rec, r.notice)
		}
		if err != nil {
			r.notice("the run log was not written: %v", err)
		}
	}
	return status
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
		var rotateErr error
		skip := inSection(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), logRotateWait)
			defer cancel()
			sec, err := st.Enter(ctx)
			if err != nil {
				return nil // busy: append without rotating
			}
			defer sec.Leave()
			_, understood, err := sec.Format(testhooks.Formats())
			if !understood {
				if err != nil {
					return err // malformed FORMAT
				}
				return errFormatChanged
			}
			if err == nil {
				// A section entered to rotate reconciles first when it is due, like every entry
				// other than admission (PROTOCOL.md section 10.4).
				_, err = sec.ReconcileIfDue(store.NowNS())
			}
			if err == nil {
				err = sec.RotateLogIfNeeded()
			}
			rotateErr = err
			return nil
		})
		if skip != nil {
			return skip
		}
		if rotateErr != nil {
			notice("the run log was not rotated: %v", rotateErr)
		}
	}
	if _, err := formatStillUnderstood(st.Dir()); err != nil {
		return err
	}
	return st.AppendRun(rec)
}
