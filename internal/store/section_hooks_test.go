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
)

// hookRecorder checks the contract of SetSectionHooks while a test runs: every time this process
// takes the lock of admit.lock, the hooks have entered; leave runs once per enter; and when the
// last leave runs, no description this process took the lock through is still open.
type hookRecorder struct {
	mu     sync.Mutex
	depth  int
	max    int
	enters int
	leaves int
	tries  int        // tries to take admit.lock's lock
	open   []*os.File // descriptions that took admit.lock's lock
	errs   []string
}

func recordHooks(t *testing.T) *hookRecorder {
	t.Helper()
	r := &hookRecorder{}
	old := flockNB
	flockNB = func(f *os.File) (bool, error) {
		ok, err := old(f)
		if filepath.Base(f.Name()) != AdmitLock {
			return ok, err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.tries++
		if r.depth == 0 {
			r.errs = append(r.errs, "admit.lock was tried outside the section hooks")
		}
		if ok {
			r.open = append(r.open, f)
		}
		return ok, err
	}
	SetSectionHooks(func() {
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
			if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
				r.errs = append(r.errs, "the last leave ran while admit.lock was still held")
			}
		}
		r.open = nil
	})
	t.Cleanup(func() {
		flockNB = old
		sectionHooks.Store(nil)
	})
	return r
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

// The section hooks bracket every try to take admit.lock and every moment it is held, in every
// way this package takes it, and only those: Enter's sleeps between busy tries lie outside them.
// Mutations: drop hookEnter from Enter, TryEnter or SectionHeld (admit.lock tried outside the
// hooks); drop hookLeave from Leave (the hooks left at depth 1); run Leave's hookLeave before the
// close (the last leave while held); drop the hookLeave after Enter's busy try, or bracket the
// whole of Enter (enters no longer match tries).
func TestSectionHooksBracketEveryHold(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	r := recordHooks(t)
	want := func(what string, wantEnters, wantDeepest int) {
		t.Helper()
		enters, deepest, tries := r.check(t)
		if enters != wantEnters || deepest != wantDeepest || tries != wantEnters {
			t.Errorf("%s: %d enters at depth up to %d for %d tries of admit.lock; want %d at depth %d, one per try",
				what, enters, deepest, tries, wantEnters, wantDeepest)
		}
	}

	sec, err := s.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sec.Leave()
	sec.Leave() // idempotent, and so is its leave
	want("Enter of a free section", 1, 1)

	sec, ok, err := s.TryEnter()
	if err != nil || !ok {
		t.Fatalf("TryEnter = %v, %v", ok, err)
	}
	sec.Leave()
	want("TryEnter of a free section", 1, 1)

	if held, err := s.SectionHeld(); err != nil || held {
		t.Fatalf("SectionHeld of a free section = %v, %v", held, err)
	}
	want("SectionHeld of a free section", 1, 1)

	// A probe while this process holds the section nests inside its hold.
	sec, err = s.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if held, err := s.SectionHeld(); err != nil || !held {
		t.Fatalf("SectionHeld inside the section = %v, %v", held, err)
	}
	sec.Leave()
	want("SectionHeld inside the section", 2, 2)

	if err := s.ClearDegraded(); err != nil {
		t.Fatal(err)
	}
	want("ClearDegraded", 1, 1)

	// Another process holds the section: every busy try is bracketed on its own, and the hooks
	// are left between tries.
	startHelper(t, "section", dir, "", nil)
	if _, ok, err := s.TryEnter(); err != nil || ok {
		t.Fatalf("TryEnter of a held section = %v, %v", ok, err)
	}
	want("TryEnter of a held section", 1, 1)
	if held, err := s.SectionHeld(); err != nil || !held {
		t.Fatalf("SectionHeld of a held section = %v, %v", held, err)
	}
	want("SectionHeld of a held section", 1, 1)
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

// The hooks may be entered from several goroutines at once: the pairs overlap, and each leave
// still matches an enter.
func TestSectionHooksOverlapAcrossGoroutines(t *testing.T) {
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
