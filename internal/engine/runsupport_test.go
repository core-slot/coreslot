//go:build unix

package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Nezhinskiy/coreslot/internal/store"
)

// EnqueueContext gives up when ctx ends while another process holds the section: it returns
// the context's error, publishes nothing and leaves nothing running.
func TestEnqueueContextEndsWhileSectionIsHeld(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, nil)
	enqueue(t, e, need("cpu=1")) // FORMAT and queue/ exist
	startHelper(t, "section", dir, "")
	before, _, _ := e.st.List(store.QueueDir)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	tk, err := e.EnqueueContext(ctx, need("cpu=1"))
	if !errors.Is(err, context.DeadlineExceeded) || tk != nil {
		t.Fatalf("EnqueueContext = %v, %v; want the context's deadline and no ticket", tk, err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("returned after %v, want soon after the 300 ms deadline", took)
	}
	if after, _, _ := e.st.List(store.QueueDir); len(after) != len(before) {
		t.Errorf("queue %v -> %v: a ticket was published", before, after)
	}
}

// Every operation that waits for the section as long as its caller lets it (enqueue, adoption,
// cancellation) gives up when ctx ends while a process holds the section, for example one
// stopped inside it: it changes nothing, and its error matches the context's and names the pid
// admit.lock records.
func TestBoundedEntriesNameTheSectionHolder(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, nil)
	tk := enqueue(t, e, need("cpu=1")) // FORMAT and queue/ exist
	h := startHelper(t, "section-marked", dir, "")
	pid := h.cmd.Process.Pid
	before := listing(t, dir)
	for _, c := range []struct {
		name string
		op   func(ctx context.Context) error
	}{
		{"EnqueueContext", func(ctx context.Context) error { _, err := e.EnqueueContext(ctx, need("cpu=1")); return err }},
		{"AdoptContext", func(ctx context.Context) error { _, _, err := e.AdoptContext(ctx, need("cpu=1")); return err }},
		{"CancelContext", func(ctx context.Context) error { return e.CancelContext(ctx, tk.ID()) }},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		start := time.Now()
		err := c.op(ctx)
		cancel()
		var busy *SectionBusyError
		if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &busy) || busy.PID != pid ||
			!strings.Contains(err.Error(), fmt.Sprintf("pid %d", pid)) {
			t.Errorf("%s with the section held by pid %d: %v; want a *SectionBusyError naming it, matching the deadline", c.name, pid, err)
		}
		if _, ok := StateReason(err); ok {
			t.Errorf("%s: %v is classified as a state error", c.name, err)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("%s returned after %v, want soon after the 300 ms deadline", c.name, took)
		}
	}
	if after := listing(t, dir); after != before {
		t.Errorf("the state directory changed:\n%s\n->\n%s", before, after)
	}
}

// UnbudgetedHold publishes the unbudgeted hold of a run outside the budget that is not a state
// error (a timeout with --on-timeout run): status shows it, and nothing is recorded as degraded.
func TestUnbudgetedHoldRecordsNoDegraded(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	if _, err := e.UnbudgetedHold(need("cpu=2"), "format"); err == nil {
		t.Error(`UnbudgetedHold with the reason "format": want an error`)
	}
	before := readGen(t, e)
	g, err := e.UnbudgetedHold(need("cpu=2"), "timeout")
	if err != nil {
		t.Fatal(err)
	}
	h := readHold(t, e, g)
	if !h.Unbudgeted || len(h.Grant) != 0 || !reflect.DeepEqual(h.UnbudgetedResources, []string{"cpu"}) {
		t.Errorf("hold %+v, want unbudgeted, empty grant, unbudgeted_resources [cpu]", h)
	}
	if readGen(t, e) != before+1 {
		t.Errorf("gen %d -> %d, want one bump", before, readGen(t, e))
	}
	if env := strings.Join(g.Env(), " "); env != "CORESLOT_GRANTED_CPU=2 CORESLOT_HELD_FD=3 CORESLOT_UNBUDGETED=timeout" {
		t.Errorf("env %q", env)
	}
	if rec, err := e.st.Degraded(); rec != nil || err != nil {
		t.Errorf("degraded %+v, %v; want no record", rec, err)
	}
	snap, err := e.Snapshot()
	if err != nil || len(snap.Holds) != 1 || !snap.Holds[0].Unbudgeted {
		t.Errorf("snapshot holds %+v, %v; want the unbudgeted hold", snap.Holds, err)
	}
	g.Close()
	if entries, _, _ := e.st.List(store.HoldsDir); len(entries) != 0 {
		t.Errorf("holds after Close: %v", entries)
	}
}

// A configuration the snapshot cannot use is reported by its reason alone: the caller prints it
// under its own heading, so it carries no "coreslot state error:" prefix.
func TestSnapshotConfigErrorIsTheReason(t *testing.T) {
	dir := newStateDir(t, "[resources]\ncpu = 0\n")
	r, err := newEngine(t, dir, nil).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if r.ConfigError == "" || strings.Contains(r.ConfigError, "state error") || !strings.Contains(r.ConfigError, "cpu") {
		t.Fatalf("ConfigError %q: want the configuration's reason without the state error's prefix", r.ConfigError)
	}
}
