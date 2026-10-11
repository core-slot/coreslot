//go:build unix

package engine

import (
	"context"
	"errors"
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
