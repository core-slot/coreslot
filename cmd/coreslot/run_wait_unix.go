//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// errNotAtOnce is the outcome of a --no-wait attempt that was not admitted.
var errNotAtOnce = errors.New("not admitted at once")

// tryOnce is --no-wait's single attempt: one Step, each of whose section entries waits at most
// RefreshEvery/4 = 0.5 s (the engine's sectionWait). The 1 s of noWaitFor bounds only enqueue.
func (r *run) tryOnce(ctx context.Context, t *engine.Ticket) (*engine.Grant, error) {
	res, err := ticketStepper{t}.step(ctx)
	if err != nil {
		return nil, err
	}
	if a, ok := res.(engine.Admitted); ok {
		return a.Grant, nil
	}
	return nil, errNotAtOnce
}

// stepper is the part of a ticket the wait loop drives: a Step, how long until the next one
// has work due, and the live holds for the waiting line.
type stepper interface {
	step(ctx context.Context) (engine.StepResult, error)
	due() time.Duration
	holders() ([]engine.HolderInfo, error)
}

// ticketStepper steps a real ticket.
type ticketStepper struct{ t *engine.Ticket }

func (s ticketStepper) step(ctx context.Context) (engine.StepResult, error) { return s.t.Step(ctx) }

func (s ticketStepper) due() time.Duration { return s.t.Due() }

func (s ticketStepper) holders() ([]engine.HolderInfo, error) { return s.t.Holders() }

// waitOneP runs wait with the Go runtime limited to one P (runtime.GOMAXPROCS(1)) and restores
// the previous setting afterwards. A waiting process sleeps almost all the time, and with one P
// its wake-ups do not wake idle threads of the other Ps, which cuts a waiter's own cost
// (bench/floor measures it). Only this process changes: GOMAXPROCS is not exported to the command.
func waitOneP(wait func() (*engine.Grant, error)) (*engine.Grant, error) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	return wait()
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

// freeMeter measures how long a run waited while the kernel's headroom was at least the memory
// it needs. It reads the headroom at most once a second, so that a waiter on the head's backoff
// stays cheap, and counts an interval as free when the reading at its start was. It informs the
// run log only; no decision reads it.
type freeMeter struct {
	needMiB  int64
	headroom func() (mib int64, ok bool)
	read     bool          // a reading was taken
	last     time.Duration // when
	free     bool          // and whether it was at least needMiB
	sum      time.Duration
}

// observe accounts for the time up to now and reads the headroom again, once a second has passed
// since the last reading. A nil meter does nothing.
func (m *freeMeter) observe(now time.Duration) {
	if m == nil || m.read && now-m.last < time.Second {
		return
	}
	if m.read && m.free {
		m.sum += now - m.last
	}
	mib, ok := m.headroom()
	m.read, m.last, m.free = true, now, ok && mib >= m.needMiB
}

// admit accounts for the time from the last reading to admission, however short, without
// reading again. A nil meter does nothing.
func (m *freeMeter) admit(now time.Duration) {
	if m != nil && m.read && m.free {
		m.sum += now - m.last
	}
}

// total is the time counted as free.
func (m *freeMeter) total() time.Duration { return m.sum }

// waitLoop steps the ticket whenever it next has work due (once per second while it is not the
// head, on the admission backoff while it is) until it is admitted, ctx ends or the budget
// fails, and writes a waiting line to w when the wait starts and every 30 s after, with the live
// holds listed then.
// After every waiting step it lets m observe the headroom, and at admission it lets m count the
// tail since the last reading.
func waitLoop(ctx context.Context, s stepper, clk waitClock, w io.Writer, line func(engine.Waiting, []engine.HolderInfo, time.Duration) string, m *freeMeter) (*engine.Grant, error) {
	start := clk.Now()
	next := start
	for {
		res, err := s.step(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		switch v := res.(type) {
		case engine.Admitted:
			m.admit(clk.Now())
			return v.Grant, nil
		case engine.Waiting:
			m.observe(clk.Now())
			if now := clk.Now(); now >= next {
				holders, err := s.holders()
				if err != nil { // informative: a failure leaves them out
					holders = nil
				}
				fmt.Fprintln(w, line(v, holders, now-start))
				next = now + waitLineEvery
			}
		}
		if err := clk.Sleep(ctx, s.due()); err != nil {
			return nil, err
		}
	}
}

// waitingLine is the line a waiting run prints every 30 s, for example `coreslot: waiting for
// mem 3 GiB: position 2 (1m00s); holders: gate pid 4711 mem 1 GiB (3m12s)`, where 1m00s is how
// long the run has waited.
func (r *run) waitingLine(w engine.Waiting, holders []engine.HolderInfo, waited time.Duration) string {
	why, advice := "", ""
	if r.why != nil {
		if why = r.why(); why != "" {
			why = "; " + why
		}
	}
	if !r.advised {
		r.advised, advice = true, "; if your tool has a time limit, run this in the background or raise the limit"
	}
	return fmt.Sprintf("coreslot: waiting for %s: position %d (%s)%s%s%s%s", r.o.needText(), w.Position,
		duration(waited), why, holderList(holders, time.Now().UnixNano()), r.pipeNote(), advice)
}

// waitReason says why a waiting request is not admitted: the room it lacks, else the memory level
// that holds admissions, else that a restarted command waits for calm.
func waitReason(rep engine.Report, req spec.Request) string {
	switch w := rep.Watch; {
	case rep.Mem.Known && len(rep.Holds) > 0 && req.NeedMiB > rep.Mem.FreeMiB:
		return fmt.Sprintf("needs %s, %s free", spec.FormatMiB(req.NeedMiB), spec.FormatMiB(rep.Mem.FreeMiB))
	case w.Fresh && w.Level >= watch.Yellow:
		return "memory is " + w.Level.String()
	case req.Restart:
		return "a stopped command restarts when memory has been calm for a while"
	}
	return ""
}

// pipeNote is what the first waiting line adds when the run's stdin is a pipe: the command
// writing it may be a stage of the same pipeline that holds the memory this run waits for, and
// blocks on the full pipe until this run reads it, so the two wait on each other until a
// timeout. coreslot cannot see the other end of a pipe, so it says so once rather than deciding.
func (r *run) pipeNote() string {
	if r.pipeNoted || r.io.Stdin == nil {
		return ""
	}
	r.pipeNoted = true
	if fi, err := r.io.Stdin.Stat(); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		return ""
	}
	return "; stdin is a pipe: if the command writing it holds a grant, it may be waiting for this run " +
		"to read; run the whole pipeline under one coreslot run"
}

// holdersText describes the live holds and the section's holder, for a --no-wait refusal.
func (r *run) holdersText(e *engine.Engine) string {
	rep, err := e.Snapshot()
	if err != nil {
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

// holderList is `; holders: gate pid 4711 mem 3 GiB (3m12s), ...`, or "" without holders. Labels
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
		parts = append(parts, fmt.Sprintf("%s pid %d %s (%s)", label, h.PID, holdNeed(h),
			duration(time.Duration(nowNS-h.StartNS))))
	}
	return "; holders: " + strings.Join(parts, ", ")
}

// holdNeed is what a hold reserves for a person: `mem 3 GiB`, or `unbudgeted` for a run outside
// the budget, which reserves nothing.
func holdNeed(h engine.HolderInfo) string {
	if h.NeedMiB == 0 {
		return "unbudgeted"
	}
	return "mem " + spec.FormatMiB(h.NeedMiB)
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
