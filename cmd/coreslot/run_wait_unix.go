//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// errNotAtOnce is the outcome of a --no-wait attempt that was not admitted.
var errNotAtOnce = errors.New("not admitted at once")

// tryOnce is --no-wait's single attempt: one Step, each of whose section entries waits at most
// RefreshEvery/4 = 0.5 s (the engine's sectionWait). The 1 s of noWaitFor bounds only enqueue.
func (r *run) tryOnce(ctx context.Context, t *engine.Ticket) (*engine.Grant, error) {
	res, err := sectionStepper{t}.step(ctx)
	if err != nil {
		return nil, err
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
	return t, err
}

// stepper is the part of a ticket the wait loop drives: a Step, how long until the next one
// has work due, and the live holds for the waiting line.
type stepper interface {
	step(ctx context.Context) (engine.StepResult, error)
	due() time.Duration
	holders() ([]engine.HolderInfo, error)
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

func (s sectionStepper) due() time.Duration { return s.t.Due() }

func (s sectionStepper) holders() ([]engine.HolderInfo, error) { return s.t.Holders() }

// waitOneP runs wait with the Go runtime limited to one P (runtime.GOMAXPROCS(1)) and restores
// the previous setting afterwards. A waiting process sleeps almost all the time, and with one P
// its wake-ups do not wake idle threads of the other Ps: a CLI waiter in a quiet queue measured
// 268 us of CPU per second against 453 us before, in one session (evidence/README.md, engine
// round 2). Only this process changes: GOMAXPROCS is not exported to the command.
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

// waitLoop steps the ticket whenever it next has work due (once per second while it is not the
// head, on the admission backoff while it is) until it is admitted, ctx ends or the budget
// fails, and writes a waiting line to w every 30 s of waiting, with the live holds listed then.
func waitLoop(ctx context.Context, s stepper, clk waitClock, w io.Writer, line func(engine.Waiting, []engine.HolderInfo, time.Duration) string) (*engine.Grant, error) {
	start := clk.Now()
	next := start + waitLineEvery
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
			return v.Grant, nil
		case engine.Waiting:
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
// cpu 4..12: position 2 (1m00s); holders: gate pid 4711 cpu12 (3m12s)`, where 1m00s is how long
// the run has waited.
func (r *run) waitingLine(w engine.Waiting, holders []engine.HolderInfo, waited time.Duration) string {
	if w.Position == 0 {
		return fmt.Sprintf("coreslot: waiting for %s: token-only mode, the state directory's format is not implemented here (%s)",
			r.o.needText(), duration(waited))
	}
	return fmt.Sprintf("coreslot: waiting for %s: position %d (%s)%s", r.o.needText(), w.Position,
		duration(waited), holderList(holders, time.Now().UnixNano()))
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
