//go:build unix

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// hookRecorder checks the contract of SetLockHooks while a test runs: every lock this process
// tries to take, of whatever file, is tried while the hooks have entered; leave runs once per
// enter; and when the last leave runs, every description that took a lock is closed again,
// except those the test declared owned (a ticket, a hold, granted tokens), which outlive the
// section by design.
type hookRecorder struct {
	mu     sync.Mutex
	depth  int
	max    int
	enters int
	leaves int
	tries  int               // tries to take a lock
	open   []*os.File        // descriptions that took a lock
	owned  map[*os.File]bool // descriptions the test keeps on purpose
	errs   []string
}

func recordHooks(t *testing.T) *hookRecorder {
	t.Helper()
	r := &hookRecorder{owned: map[*os.File]bool{}}
	old := flockNB
	flockNB = func(f *os.File) (bool, error) {
		ok, err := old(f)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.tries++
		if r.depth == 0 {
			r.errs = append(r.errs, filepath.Base(f.Name())+" was tried outside the lock hooks")
		}
		if ok {
			r.open = append(r.open, f)
		}
		return ok, err
	}
	SetLockHooks(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.depth++
		r.enters++
		r.max = max(r.max, r.depth)
	}, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.depth--
		r.leaves++
		if r.depth < 0 {
			r.errs = append(r.errs, "leave ran without an enter")
		}
		if r.depth > 0 {
			return
		}
		for _, f := range r.open {
			if _, err := f.Stat(); !r.owned[f] && !errors.Is(err, os.ErrClosed) {
				r.errs = append(r.errs, "the last leave ran while "+filepath.Base(f.Name())+" was still locked")
			}
		}
		r.open = nil
	})
	t.Cleanup(func() {
		flockNB = old
		lockHooks.Store(nil)
	})
	return r
}

// own declares fs kept on purpose: their locks outlive the hooks.
func (r *hookRecorder) own(fs ...*os.File) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range fs {
		r.owned[f] = true
	}
}

// check fails the test unless every enter has had its leave and nothing broke the contract, and
// returns the counts: enters, the deepest nesting and the tries.
func (r *hookRecorder) check(t *testing.T) (enters, deepest, tries int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.errs {
		t.Error(e)
	}
	if r.depth != 0 || r.enters != r.leaves {
		t.Errorf("%d enters, %d leaves: the hooks were left at depth %d", r.enters, r.leaves, r.depth)
	}
	enters, deepest, tries = r.enters, r.max, r.tries
	r.enters, r.leaves, r.max, r.tries, r.errs = 0, 0, 0, 0, nil
	return enters, deepest, tries
}

// want checks the contract and that the hooks entered wantEnters times, nested at most
// wantDeepest deep, for wantTries tries.
func (r *hookRecorder) want(t *testing.T, what string, wantEnters, wantDeepest, wantTries int) {
	t.Helper()
	enters, deepest, tries := r.check(t)
	if enters != wantEnters || deepest != wantDeepest || tries != wantTries {
		t.Errorf("%s: %d enters at depth up to %d for %d tries; want %d at depth %d for %d tries",
			what, enters, deepest, tries, wantEnters, wantDeepest, wantTries)
	}
}

// The lock hooks bracket every try to take admit.lock and every moment it is held, in every
// way this package takes it, and only those: Enter's sleeps between busy tries lie outside them.
// Mutations: drop hookEnter from Enter, TryEnter or SectionHeld (admit.lock tried outside the
// hooks); drop hookLeave from Leave (the hooks left at depth 1); run Leave's hookLeave before the
// close (the last leave while held); drop the hookLeave after Enter's busy try, or bracket the
// whole of Enter (enters no longer match tries).
func TestLockHooksBracketEverySectionHold(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	r := recordHooks(t)

	sec, err := s.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sec.Leave()
	sec.Leave() // idempotent, and so is its leave
	r.want(t, "Enter of a free section", 1, 1, 1)

	sec, ok, err := s.TryEnter()
	if err != nil || !ok {
		t.Fatalf("TryEnter = %v, %v", ok, err)
	}
	sec.Leave()
	r.want(t, "TryEnter of a free section", 1, 1, 1)

	if held, err := s.SectionHeld(); err != nil || held {
		t.Fatalf("SectionHeld of a free section = %v, %v", held, err)
	}
	r.want(t, "SectionHeld of a free section", 1, 1, 1)

	// A probe while this process holds the section nests inside its hold.
	sec, err = s.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if held, err := s.SectionHeld(); err != nil || !held {
		t.Fatalf("SectionHeld inside the section = %v, %v", held, err)
	}
	sec.Leave()
	r.want(t, "SectionHeld inside the section", 2, 2, 2)

	if err := s.ClearDegraded(); err != nil {
		t.Fatal(err)
	}
	r.want(t, "ClearDegraded", 1, 1, 1)

	// Another process holds the section: every busy try is bracketed on its own, and the hooks
	// are left between tries.
	startHelper(t, "section", dir, "", nil)
	if _, ok, err := s.TryEnter(); err != nil || ok {
		t.Fatalf("TryEnter of a held section = %v, %v", ok, err)
	}
	r.want(t, "TryEnter of a held section", 1, 1, 1)
	if held, err := s.SectionHeld(); err != nil || !held {
		t.Fatalf("SectionHeld of a held section = %v, %v", held, err)
	}
	r.want(t, "SectionHeld of a held section", 1, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := s.Enter(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Enter of a held section = %v, want the deadline", err)
	}
	enters, deepest, tries := r.check(t)
	if enters < 2 || deepest != 1 || tries != enters {
		t.Errorf("Enter of a held section: %d enters at depth up to %d for %d tries; want one per try, at least 2, at depth 1",
			enters, deepest, tries)
	}
}

// The lock hooks bracket every probe of a token, ticket or hold, each probe on its own, outside
// the section as inside it, and every lock this package takes is tried inside them; the locks a
// client owns (its ticket, its hold, its granted tokens) are taken inside the section's bracket
// and are the only ones that outlive it. A process stopped while it holds a probe lock makes
// every other client count that token as locked or that ticket or hold as live, which the
// section's bracket alone did not prevent.
// Mutations: drop hookEnter from ProbeLocked or removeIfDead (a token, ticket or hold tried
// outside the hooks); run ProbeLocked's hookLeave before its close, or defer removeIfDead's after
// the close's defer (the last leave while a probe lock is held); drop either hookLeave (the
// hooks left deeper than 0).
func TestLockHooksBracketEveryProbe(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)

	// Before recording: three token files of cpu, one of them locked by a description of the
	// test's own, as another client's grant would be.
	sec, err := s.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ts, got, err := sec.TakeTokens(map[string]int{"cpu": 3}, map[string]int{"cpu": 3})
	if err != nil || got["cpu"] != 3 {
		t.Fatalf("TakeTokens = %v, %v", got, err)
	}
	sec.Leave()
	ts.Close()
	other := lockOther(t, filepath.Join(dir, ResDir, "cpu", "1"))

	r := recordHooks(t)

	// Outside the section: the reader's probes (status, doctor, a cancel by label, the --no-wait
	// refusal, a library Step) and the election's.
	if n, err := s.CountLockedProbe("cpu"); err != nil || n != 1 {
		t.Fatalf("CountLockedProbe = %d, %v; want 1", n, err)
	}
	r.want(t, "CountLockedProbe of 3 tokens", 3, 1, 3)
	if locked, err := ProbeLocked(other.Name()); err != nil || !locked {
		t.Fatalf("ProbeLocked of a locked token = %v, %v", locked, err)
	}
	r.want(t, "ProbeLocked of a locked token", 1, 1, 1)

	// Inside the section: publication of an owned ticket and hold, the count and the take of
	// admission, the removals that probe before they unlink, and reconciliation.
	sec, err = s.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := sec.CountLocked("cpu"); err != nil || n != 1 {
		t.Fatalf("CountLocked = %d, %v; want 1", n, err)
	}
	ticket, tname, _, err := sec.PublishNamed(QueueDir, []byte(`{}`), 1, NextSeq, ticketName)
	if err != nil {
		t.Fatal(err)
	}
	hold, hname, err := sec.PublishHold(wire.Hold{StartNS: 1791720600000000000, Label: "gate:unit"}, 4711, 1)
	if err != nil {
		t.Fatal(err)
	}
	grant, got, err := sec.TakeTokens(map[string]int{"cpu": 2}, map[string]int{"cpu": 3})
	if err != nil || got["cpu"] != 2 {
		t.Fatalf("TakeTokens = %v, %v", got, err)
	}
	r.own(ticket, hold)
	r.own(grant.Files...)
	sec.Leave()
	// 1 section + 3 probes; tries: 1 + 3 + the ticket's and the hold's locks + 3 tries of tokens
	// (one of them busy).
	r.want(t, "a section that counts, publishes and takes", 4, 2, 1+3+2+3)

	if dead, err := s.Probe(QueueDir, tname); err != nil || dead {
		t.Fatalf("Probe of a live ticket = %v, %v", dead, err)
	}
	if dead, err := s.Probe(HoldsDir, hname); err != nil || dead {
		t.Fatalf("Probe of a live hold = %v, %v", dead, err)
	}
	r.want(t, "Probe of a live ticket and hold", 2, 1, 2)

	// The owner lets go of its ticket and hold, then removes them by probe-before-unlink.
	ticket.Close()
	hold.Close()
	sec, err = s.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := sec.RemoveOwnTicket(tname); err != nil || !removed {
		t.Fatalf("RemoveOwnTicket = %v, %v", removed, err)
	}
	if removed, err := sec.ProbeAndRemoveHold(hname); err != nil || !removed {
		t.Fatalf("ProbeAndRemoveHold = %v, %v", removed, err)
	}
	sec.Leave()
	r.want(t, "RemoveOwnTicket and ProbeAndRemoveHold", 3, 2, 3)

	// Reconciliation: one dead ticket and one dead hold, probed and unlinked under their probe
	// locks.
	sec, err = s.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{QueueDir, HoldsDir} {
		f, _, _, err := sec.PublishNamed(dir, []byte(`{}`), 2, NextSeq, ticketName)
		if err != nil {
			t.Fatal(err)
		}
		f.Close() // dead
	}
	if removed, err := sec.ReconcileIfDue(NowNS() + int64(2*ReconcileEvery)); err != nil || removed != 2 {
		t.Fatalf("ReconcileIfDue = %d, %v; want 2", removed, err)
	}
	sec.Leave()
	r.want(t, "ReconcileIfDue of a dead ticket and hold", 3, 2, 1+2+2)

	// A reset's liveness probes: the tokens it counts (one is the grant's, one another
	// description's) refuse it.
	sec, err = s.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if live, _, err := sec.Reset(); err != nil || live.Empty() {
		t.Fatalf("Reset = %+v, %v; want a refusal for the locked tokens", live, err)
	}
	sec.Leave()
	r.want(t, "Reset refused by locked tokens", 4, 2, 4)
	grant.Close()

	// Every caller of the probe-before-unlink is inside the section, whose bracket hides the
	// order of the probe's own leave and close; called alone, its leave must still come last.
	sec, err = s.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f, name, _, err := sec.PublishNamed(QueueDir, []byte(`{}`), 3, NextSeq, ticketName)
	if err != nil {
		t.Fatal(err)
	}
	r.own(f)
	sec.Leave()
	f.Close()
	r.check(t)
	if removed, gone, err := removeIfDead(filepath.Join(dir, QueueDir, name)); err != nil || !removed || gone {
		t.Fatalf("removeIfDead of a dead ticket = %v, %v, %v", removed, gone, err)
	}
	r.want(t, "removeIfDead of a dead ticket, alone", 1, 1, 1)
}

// lockOther locks path through a description of its own, outside flockNB, as another client
// would, until the test ends.
func lockOther(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return f
}

// The hooks may be entered from several goroutines at once: the pairs overlap, and each leave
// still matches an enter.
func TestLockHooksOverlapAcrossGoroutines(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	r := recordHooks(t)
	sec, err := s.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			if held, err := s.SectionHeld(); err != nil || !held {
				errs <- fmt.Errorf("SectionHeld = %v, %v", held, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	sec.Leave()
	if enters, _, tries := r.check(t); enters != 9 || tries != 9 {
		t.Errorf("%d enters for %d tries; want 9 of each", enters, tries)
	}
}
