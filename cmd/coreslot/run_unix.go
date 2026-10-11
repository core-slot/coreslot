//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/testhooks"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// Timing of a waiting run (PROTOCOL.md section 22).
const (
	pollEvery     = 250 * time.Millisecond // a Step at least this often, well inside the 2 s refresh
	waitLineEvery = 30 * time.Second       // the waiting line
	noWaitFor     = time.Second            // --no-wait: how long the section is tried
	logRotateWait = 2 * time.Second        // entering the section to rotate the run log
)

// runSignals receives the signals a run handles from its start: while it waits they interrupt
// the wait, and while its command runs the supervisor decides what reaches the command.
var runSignals = make(chan os.Signal, 16)

// handledSignals are caught for the whole run, unless the run started with one ignored. A
// caught disposition does not survive exec, so the command starts with the default ones.
var handledSignals = []os.Signal{syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP}

// run is one `coreslot run`.
type run struct {
	o       runOptions
	io      IO
	startNS int64 // wall clock at the run's start
	start   time.Time
	load    float64
	waited  time.Duration
	env     []string // the command's environment before its grant: the inherited context dropped
}

// runCommand runs `coreslot run` and returns its exit status (PROTOCOL.md section 19).
func runCommand(args []string, io IO) int {
	o, err := parseRun(args, io.Stderr)
	if errors.Is(err, errHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: %v\n", err)
		return exitUsage
	}
	signal.Notify(runSignals, notIgnored(handledSignals...)...)
	defer signal.Stop(runSignals)
	installStops()
	r := &run{o: o, io: io, startNS: time.Now().UnixNano(), start: time.Now(), load: load1()}
	return r.decide()
}

func (r *run) notice(format string, args ...any) {
	fmt.Fprintf(r.io.Stderr, "coreslot: "+format+"\n", args...)
}

// refuse prints why the run refuses and returns the refusal code.
func (r *run) refuse(format string, args ...any) int {
	r.notice(format, args...)
	return r.o.refusalCode
}

// openEngine opens the state directory as the library's Open does: CORESLOT_STATE_DIR (with
// its configuration at <statedir>/config.toml) or the default of PROTOCOL.md section 4.1.
func openEngine() (*engine.Engine, string, error) {
	dir, override, err := config.StateDir()
	if err != nil {
		return nil, "", &stateError{Reason: err.Error()}
	}
	cfgPath := filepath.Join(dir, store.ConfigFile)
	if !override {
		if cfgPath, err = config.Path(); err != nil {
			return nil, "", &stateError{Reason: err.Error()}
		}
	}
	st, err := store.Open(dir)
	if err != nil {
		return nil, "", asState(err)
	}
	return engine.New(st, &config.Watcher{Path: cfgPath}, config.MachineCPUs, engine.Clock{}), cfgPath, nil
}

// asState turns an error from the budget into a state error, keeping the classes the run acts
// on: usage, cancellation and context ends. Whatever else stops the budget from working makes
// the run fail open (PROTOCOL.md section 14).
func asState(err error) error {
	var usage *spec.UsageError
	if err == nil || errors.As(err, &usage) || errors.Is(err, engine.ErrCancelled) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var se *store.StateError
	if errors.As(err, &se) {
		return &stateError{Reason: se.Reason}
	}
	var own *stateError
	if errors.As(err, &own) {
		return err
	}
	return &stateError{Reason: err.Error()}
}

func isStoreStateError(err error) bool {
	var se *store.StateError
	return errors.As(err, &se)
}

// priority is the request's priority: --priority, or else the configuration's [priorities]
// entry of the longest prefix of the label, or 0. A configuration that cannot be read is a
// state error.
func (r *run) priority(cfgPath, stateDir string) (int, error) {
	if filepath.Dir(cfgPath) == stateDir {
		if err := store.CheckEntry(cfgPath, false); err != nil {
			return 0, asState(err)
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return 0, &stateError{Reason: err.Error()}
	}
	if r.o.prioritySet {
		return r.o.priority, nil
	}
	p, _ := cfg.PriorityFor(r.o.label)
	return p, nil
}

// ordinary is a run that queues (PROTOCOL.md section 18 rule 3).
func (r *run) ordinary() int {
	e, cfgPath, err := openEngine()
	if err != nil {
		return r.failOpen(nil, err)
	}
	prio, err := r.priority(cfgPath, e.Store().Dir())
	if err != nil {
		return r.failOpen(e, err)
	}
	req := r.o.request(prio)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if r.o.timeout > 0 {
		var c context.CancelFunc
		ctx, c = context.WithTimeout(ctx, r.o.timeout)
		defer c()
	}
	watch := watchSignals(cancel)

	enqCtx := ctx
	if r.o.noWait {
		var c context.CancelFunc
		enqCtx, c = context.WithTimeout(ctx, noWaitFor)
		defer c()
	}
	t, err := enqueue(enqCtx, e, req)
	var g *engine.Grant
	if err == nil {
		if r.o.noWait {
			g, err = r.tryOnce(ctx, t)
		} else {
			g, err = waitLoop(ctx, sectionStepper{t}, newRealClock(), r.io.Stderr, r.waitingLine)
		}
		if err != nil {
			_ = inSection(t.Withdraw)
		}
	}
	r.waited = time.Since(r.start)
	if sig := watch.end(); sig != nil {
		if g != nil {
			_ = inSection(g.Close)
		}
		if sig == syscall.SIGINT {
			r.notice("interrupted while waiting")
		}
		code, _ := classify(&interruptError{Sig: sig.(syscall.Signal)})
		return code
	}
	if err != nil {
		return r.notAdmitted(e, t, err)
	}
	return r.admitted(e, g)
}

// notAdmitted handles a wait that ended without a grant.
func (r *run) notAdmitted(e *engine.Engine, t *engine.Ticket, err error) int {
	switch {
	case errors.Is(err, engine.ErrCancelled):
		return r.refuse("the request was cancelled")
	case r.o.noWait && (errors.Is(err, errNotAtOnce) || errors.Is(err, context.DeadlineExceeded)):
		r.notice("not admitted at once (--no-wait)%s", r.holdersText(e))
		return r.o.refusalCode
	case errors.Is(err, context.DeadlineExceeded):
		if r.o.onTimeout == "run" {
			return r.timeoutRun(e, t)
		}
		return r.refuse("not admitted within %v (--timeout); refusing", r.o.timeout)
	}
	return r.failOpen(e, err)
}

// errNotAtOnce is the outcome of a --no-wait attempt that was not admitted.
var errNotAtOnce = errors.New("not admitted at once")

// tryOnce is --no-wait's single attempt: one Step, whose section entry waits at most 1 s.
func (r *run) tryOnce(ctx context.Context, t *engine.Ticket) (*engine.Grant, error) {
	res, err := sectionStepper{t}.step(ctx)
	if err != nil {
		return nil, asState(err)
	}
	if a, ok := res.(engine.Admitted); ok {
		return a.Grant, nil
	}
	return nil, errNotAtOnce
}

// enqueue publishes the request's ticket, waiting for the section only until ctx ends; it then
// returns the context's error having published nothing.
func enqueue(ctx context.Context, e *engine.Engine, req spec.Request) (t *engine.Ticket, err error) {
	err = inSection(func() (err error) {
		t, err = e.EnqueueContext(ctx, req)
		return err
	})
	return t, asState(err)
}

// stepper is the part of a ticket the wait loop drives.
type stepper interface {
	step(ctx context.Context) (engine.StepResult, error)
}

// sectionStepper steps a ticket inside inSection: a Step may hold the admission section.
type sectionStepper struct{ t *engine.Ticket }

func (s sectionStepper) step(ctx context.Context) (res engine.StepResult, err error) {
	err = inSection(func() error {
		res, err = s.t.Step(ctx)
		return err
	})
	return res, err
}

// waitClock is the wait loop's clock: monotonic time and a sleep that ends with ctx.
type waitClock interface {
	Now() time.Duration
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{ start time.Time }

func newRealClock() realClock { return realClock{start: time.Now()} }

func (c realClock) Now() time.Duration { return time.Since(c.start) }

func (c realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// waitLoop steps the ticket every 250 ms until it is admitted, ctx ends or the budget fails,
// and writes a waiting line to w every 30 s of waiting.
func waitLoop(ctx context.Context, s stepper, clk waitClock, w io.Writer, line func(engine.Waiting, time.Duration) string) (*engine.Grant, error) {
	start := clk.Now()
	next := start + waitLineEvery
	for {
		res, err := s.step(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, asState(err)
		}
		switch v := res.(type) {
		case engine.Admitted:
			return v.Grant, nil
		case engine.Waiting:
			if now := clk.Now(); now >= next {
				fmt.Fprintln(w, line(v, now-start))
				next = now + waitLineEvery
			}
		}
		if err := clk.Sleep(ctx, pollEvery); err != nil {
			return nil, err
		}
	}
}

// waitingLine is the line a waiting run prints every 30 s, for example `coreslot: waiting for
// cpu 4..12: position 2; holders: gate pid 4711 cpu12 (3m12s)`.
func (r *run) waitingLine(w engine.Waiting, waited time.Duration) string {
	if w.Position == 0 {
		return fmt.Sprintf("coreslot: waiting for %s: token-only mode, the state directory's format is not implemented here (%s)",
			r.o.needText(), duration(waited))
	}
	return fmt.Sprintf("coreslot: waiting for %s: position %d (%s)%s", r.o.needText(), w.Position,
		duration(waited), holderList(w.Holders, time.Now().UnixNano()))
}

// holdersText describes the live holds and the section's holder, for a --no-wait refusal.
func (r *run) holdersText(e *engine.Engine) string {
	var rep engine.Report
	if err := inSection(func() (err error) { // Snapshot probes admit.lock
		rep, err = e.Snapshot()
		return err
	}); err != nil {
		return ""
	}
	s := holderList(rep.Holds, time.Now().UnixNano())
	if rep.Section.Held && rep.Section.PID != 0 {
		s += fmt.Sprintf("; the admission section is held by pid %d", rep.Section.PID)
	} else if rep.Section.Held {
		s += "; the admission section is held"
	}
	return s
}

// holderList is `; holders: gate pid 4711 cpu12 (3m12s), ...`, or "" without holders. Labels
// are escaped so that each holder stays on the line.
func holderList(holders []engine.HolderInfo, nowNS int64) string {
	if len(holders) == 0 {
		return ""
	}
	parts := make([]string, 0, len(holders))
	for _, h := range holders {
		label := wire.EscapeHuman(h.Label)
		if label == "" {
			label = "(no label)"
		}
		var grant []string
		for _, name := range slices.Sorted(maps.Keys(h.Grant)) {
			grant = append(grant, name+strconv.Itoa(h.Grant[name]))
		}
		if h.Unbudgeted {
			grant = append(grant, "unbudgeted")
		}
		parts = append(parts, fmt.Sprintf("%s pid %d %s (%s)", label, h.PID, strings.Join(grant, " "),
			duration(time.Duration(nowNS-h.StartNS))))
	}
	return "; holders: " + strings.Join(parts, ", ")
}

// duration is a coarse duration: 12s, 3m12s, 1h02m03s.
func duration(d time.Duration) string {
	s := int64(max(d, 0) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm%02ds", s/3600, s%3600/60, s%60)
}

// sigWatch turns a signal received while the run waits into the end of the wait.
type sigWatch struct {
	stop, done chan struct{}
	sig        os.Signal
}

func watchSignals(cancel context.CancelFunc) *sigWatch {
	w := &sigWatch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		select {
		case w.sig = <-runSignals:
			cancel()
		case <-w.stop:
		}
	}()
	return w
}

// end stops watching and returns the signal that ended the wait, or nil.
func (w *sigWatch) end() os.Signal {
	close(w.stop)
	<-w.done
	return w.sig
}

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
		release:    func() { _ = inSection(g.Close) },
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
	c := child{grant: r.o.minGrant(), unbudgeted: r.o.sortedNeed(), failOpen: reason, release: func() {}}
	var g *engine.Grant
	var ferr error
	if e != nil {
		ferr = inSection(func() (err error) {
			g, err = e.FailOpen(r.o.request(0), reason)
			return err
		})
	}
	if g != nil {
		c.env, c.files, c.release = append(slices.Clone(r.env), g.Env()...), g.Files(), func() { _ = inSection(g.Close) }
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
	c := child{grant: r.o.minGrant(), unbudgeted: r.o.sortedNeed(), failOpen: timeoutReason, release: func() {}}
	var g *engine.Grant
	herr := inSection(func() (err error) {
		g, err = e.UnbudgetedHold(r.o.request(0), timeoutReason)
		return err
	})
	if g != nil {
		c.env, c.files, c.release = append(slices.Clone(r.env), g.Env()...), g.Files(), func() { _ = inSection(g.Close) }
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

// child is what a run starts its command with.
type child struct {
	env        []string
	files      []*os.File // ExtraFiles: fd 3 (nil without a hold), then the tokens
	grant      map[string]int
	clamped    []string
	unbudgeted []string
	failOpen   string
	nested     bool
	release    func()       // closes the run's copies of the grant once the command exited
	log        *store.Store // the run log to append to; nil writes none
}

// runChild starts the command with c, waits for it, releases the grant and appends the run
// log line. It returns the command's status, or the status of a command that could not start.
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
		c.release()
		code, _ := classify(&interruptError{Sig: sig})
		return code
	}
	terminal := terminalMode(r.io.Stdin)
	prepareChild(cmd, terminal)
	started := time.Now()
	status := 0
	if err := cmd.Start(); err != nil {
		childDone(terminal)
		c.release()
		code, _ := classify(err)
		r.notice("cannot run %s: %v", argv[0], err)
		status = code
	} else {
		status = superviseChild(cmd, terminal)
		childDone(terminal)
		c.release()
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
			err = appendRunLog(c.log, rec, true)
		}
		if err != nil {
			r.notice("the run log was not written: %v", err)
		}
	}
	return status
}

// pendingSignal returns a handled signal the run received and has not acted on, or 0.
func pendingSignal() syscall.Signal {
	select {
	case s := <-runSignals:
		return s.(syscall.Signal)
	default:
		return 0
	}
}

// errFormatChanged is the outcome of a rotation section that found a FORMAT this client does
// not understand: the line is not appended (PROTOCOL.md section 14).
var errFormatChanged = errors.New("the state directory's FORMAT is no longer one this client implements")

// appendRunLog appends rec to the run log (PROTOCOL.md section 20). When the log is at or above
// 10 MiB and rotate is set, it first rotates it inside the section if it can enter within 2 s;
// when that section finds a FORMAT this client does not understand, nothing is appended. A
// busy section only skips the rotation. A pass-through never rotates (appendNestedLog).
func appendRunLog(st *store.Store, rec store.RunRecord, rotate bool) error {
	if rotate && st.RunLogNeedsRotation() {
		err := inSection(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), logRotateWait)
			defer cancel()
			sec, err := st.Enter(ctx)
			if err != nil {
				return nil // busy: append without rotating
			}
			defer sec.Leave()
			_, understood, err := sec.Format(testhooks.Formats())
			if err != nil {
				return err
			}
			if !understood {
				return errFormatChanged
			}
			return sec.RotateLogIfNeeded()
		})
		if err != nil {
			return err
		}
	}
	return st.AppendRun(rec)
}
