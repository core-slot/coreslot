//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// trimNotice is the start of the line a run prints when it first acts on a request to trim.
const trimNotice = "coreslot: the reservation of "

// trimHold admits a hold reserving needMiB in a state directory with room for it, and returns the
// engine that holds it, the grant and its store.
func trimHold(t *testing.T, needMiB int64) (*engine.Engine, *engine.Grant, *store.Store) {
	t.Helper()
	c := openLib(t, budgetDir(t, 16384))
	g := hold(t, c, "x", needMiB)
	return c, g, c.Store()
}

// requestTrim asks g's run, through c, to trim g's hold.
func requestTrim(t *testing.T, c *engine.Engine, g *engine.Grant) {
	t.Helper()
	id, ok := wire.ParseName(g.HoldName())
	if !ok {
		t.Fatalf("hold name %q", g.HoldName())
	}
	if err := c.TrimContext(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

// trimWatch runs r's watchChild for one 1 s listing per entry of mib, the listing's footprint in
// MiB, a negative entry a listing that failed. each, when set, runs before listing n. It returns
// the hold's body after each tick, the tick of listing 2 first, and the run's peak.
func trimWatch(t *testing.T, r *run, g *engine.Grant, st *store.Store, mib []int64, each func(n int)) ([]wire.Hold, peak) {
	t.Helper()
	savedTree, savedClient := listTree, listClient
	t.Cleanup(func() { listTree, listClient = savedTree, savedClient })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var bodies []wire.Hold
	n := 0
	listTree = func(int) (int64, map[int]memsample.TreeTimes, bool) {
		n++
		if n > 2 && n%2 == 1 {
			bodies = append(bodies, body(t, st, g))
		}
		if each != nil {
			each(n)
		}
		if n == len(mib) {
			cancel()
		}
		if mib[n-1] < 0 {
			return 0, nil, false
		}
		return mib[n-1] << 10, nil, true
	}
	listClient = func(int) bool { return false }
	_, p := r.watchChild(ctx, &supervisor{pid: 42, group: true}, g, st)
	return append(bodies, body(t, st, g)), p
}

// trimRun is a run whose trim window spans s seconds of listings, with its stderr in a file.
func trimRun(t *testing.T, s int64) *run {
	t.Helper()
	r := restartable("make", "test")
	r.trimWindowS = s
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stderr.Close() })
	r.io.Stderr = stderr
	return r
}

// said is what r has written to its stderr file.
func said(t *testing.T, r *run) string {
	t.Helper()
	b, err := os.ReadFile(r.io.Stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// trimOf is the need_mib of bodies, with whether each was trimmed and its asked_mib.
func trimOf(bodies []wire.Hold) (need []int64, trimmed []bool, asked []int64) {
	for _, h := range bodies {
		need, trimmed, asked = append(need, h.NeedMiB), append(trimmed, h.Trimmed), append(asked, h.AskedMiB)
	}
	return need, trimmed, asked
}

// A run reads [watch] trim_window_s at its start and follows a trim over that window: with 4 s
// its hold is lowered within seconds, where the default 60 s would keep the need asked for a
// minute. Mutation: ignore the configuration (keep the default window) → the need stays 4096
// past the wait, red.
func TestRunUsesConfiguredTrimWindow(t *testing.T) {
	dir := budgetDir(t, 16384)
	writeConfig(t, dir, "[memory]\nreserve = \"0\"\n[watch]\ntrim_window_s = 4\n")
	ready, done := filepath.Join(t.TempDir(), "ready"), filepath.Join(t.TempDir(), "done")
	args := append([]string{"run", "--restart=yes", "--label", "w", "--mem", "4G", "--"}, helper(t, "close-and-wait", ready, done)...)
	cmd := cliCmd(t, baseEnv(dir), args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the command's start", func() bool { _, err := os.Stat(ready); return err == nil })
	names := entries(t, dir, store.HoldsDir)
	if len(names) != 1 {
		t.Fatalf("holds %q, want the run's one", names)
	}
	id, _ := wire.ParseName(names[0])
	if err := openLib(t, dir).TrimContext(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var h wire.Hold
	waitFor(t, 20*time.Second, "the run lowering its reservation", func() bool {
		b, err := os.ReadFile(filepath.Join(dir, store.HoldsDir, names[0]))
		if err != nil {
			return false
		}
		if h, err = wire.DecodeHold(b); err != nil { // read again: a rewrite may be under way
			return false
		}
		return h.Trimmed && h.NeedMiB < 4096
	})
	if h.AskedMiB != 4096 {
		t.Errorf("asked_mib %d, want 4096", h.AskedMiB)
	}
	if err := os.WriteFile(done, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("run: %v", err)
	}
}

// A run whose hold no request names never rewrites need_mib, and its body has neither key,
// whatever its footprint. Mutation: write the window's need into every hold → 600, red.
func TestRunNeverRewritesNeedUntrimmed(t *testing.T) {
	_, g, st := trimHold(t, 4000)
	r := trimRun(t, 2)
	bodies, _ := trimWatch(t, r, g, st, []int64{200, 200, 200, 200}, nil)
	need, trimmed, asked := trimOf(bodies)
	if !slices.Equal(need, []int64{4000, 4000}) || !slices.Equal(trimmed, []bool{false, false}) || !slices.Equal(asked, []int64{0, 0}) {
		t.Errorf("need %v trimmed %v asked %v; want 4000 untouched, no keys", need, trimmed, asked)
	}
	raw, err := os.ReadFile(filepath.Join(st.Dir(), store.HoldsDir, g.HoldName()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "asked_mib") || strings.Contains(string(raw), "trimmed") {
		t.Errorf("hold %s, want neither key", raw)
	}
	if s := said(t, r); strings.Contains(s, trimNotice) {
		t.Errorf("stderr %q, want no trim notice", s)
	}
}

// A trimmed hold reserves the need asked until its window holds as many readings as it spans,
// then the largest reading in the window plus the margin (400 for 4000): 3400 while the 3000
// reading is in the window, then 600. The window counts readings from the attempt's start, so a
// request finds the readings already taken. Mutation: start the window at the request → 4000 at
// the second tick, red.
func TestRunTrimLowersAfterWindow(t *testing.T) {
	c, g, st := trimHold(t, 4000)
	r := trimRun(t, 4)
	each := func(n int) {
		if n == 2 { // after the first listing: the tick of the second finds it
			if h := body(t, st, g); h.Trimmed || h.AskedMiB != 0 {
				t.Errorf("before the request: trimmed %v asked %d; want neither", h.Trimmed, h.AskedMiB)
			}
			requestTrim(t, c, g)
		}
	}
	bodies, _ := trimWatch(t, r, g, st, []int64{3000, 200, 200, 200, 200, 200}, each)
	need, trimmed, asked := trimOf(bodies)
	if !slices.Equal(need, []int64{4000, 3400, 600}) || !slices.Equal(trimmed, []bool{true, true, true}) || !slices.Equal(asked, []int64{4000, 4000, 4000}) {
		t.Errorf("need %v trimmed %v asked %v; want 4000 3400 600, trimmed, asked 4000", need, trimmed, asked)
	}
}

// A trimmed reservation rises with the footprint in the same tick, up to the need asked and no
// further. Mutation: keep the need of the first trimmed write → 600 throughout, red.
func TestRunTrimRisesWithFootprint(t *testing.T) {
	c, g, st := trimHold(t, 4000)
	r := trimRun(t, 2)
	each := func(n int) {
		if n == 1 {
			requestTrim(t, c, g)
		}
	}
	bodies, _ := trimWatch(t, r, g, st, []int64{200, 200, 3500, 200, 5000, 200}, each)
	if need, _, _ := trimOf(bodies); !slices.Equal(need, []int64{600, 3900, 4000}) {
		t.Errorf("need %v; want 600 3900 4000", need)
	}
}

// A listing that failed is no reading: it neither pushes a high reading out of the window nor
// counts as a low one, so the reservation stays where it was. Mutation: add 0 for a failed
// listing → the 3000 reading leaves the window and the need drops to 600, red.
func TestRunTrimKeepsNeedThroughFailedListing(t *testing.T) {
	c, g, st := trimHold(t, 4000)
	r := trimRun(t, 2)
	each := func(n int) {
		if n == 1 {
			requestTrim(t, c, g)
		}
	}
	bodies, _ := trimWatch(t, r, g, st, []int64{200, 3000, -1, 200}, each)
	if need, trimmed, _ := trimOf(bodies); !slices.Equal(need, []int64{3400, 3400}) || !slices.Equal(trimmed, []bool{true, true}) {
		t.Errorf("need %v trimmed %v; want 3400 at both ticks", need, trimmed)
	}
}

// The run says once, and records once, that it acted on the request, with what it reserves. Mutation: say and record at every trimmed write →
// three of each, red.
func TestRunTrimEventOnce(t *testing.T) {
	b := newBed(t, 16384)
	g := hold(t, b.e, "x", 4000)
	st := b.e.Store()
	b.r.trimWindowS = 2
	each := func(n int) {
		if n == 1 {
			requestTrim(t, b.e, g)
		}
	}
	trimWatch(t, b.r, g, st, []int64{200, 300, 200, 200, 200, 200}, each)
	var trims []map[string]any
	for _, m := range events(t, b.dir) {
		if m["e"] == "trim" {
			trims = append(trims, m)
		}
	}
	if len(trims) != 1 {
		t.Fatalf("%d trim events among %q, want 1", len(trims), eventNames(events(t, b.dir)))
	}
	m := trims[0]
	if m["hold"] != g.HoldName() || m["asked_mib"] != 4000.0 || m["need_mib"] != 700.0 || m["footprint_mib"] != 300.0 {
		t.Errorf("trim event %v; want hold %s, asked 4000, need 700, footprint 300", m, g.HoldName())
	}
	if n := strings.Count(b.notices(t), trimNotice); n != 1 {
		t.Errorf("stderr %q says the trim %d times, want once", b.notices(t), n)
	}
}

// A run with telemetry off acts on the request all the same and says so; it records nothing.
// Mutation: act only with a recorder → the hold stays at 4000, untrimmed, red.
func TestRunTrimWithoutRecorder(t *testing.T) {
	c, g, st := trimHold(t, 4000)
	r := trimRun(t, 2)
	each := func(n int) {
		if n == 1 {
			requestTrim(t, c, g)
		}
	}
	bodies, _ := trimWatch(t, r, g, st, []int64{200, 200, 200, 200}, each)
	if need, trimmed, _ := trimOf(bodies); !slices.Equal(need, []int64{600, 600}) || !slices.Equal(trimmed, []bool{true, true}) {
		t.Errorf("need %v trimmed %v; want 600, trimmed", need, trimmed)
	}
	want := trimNotice + "`make test` was lowered by hand with coreslot trim: it follows what the command uses, up to the 3.9 GiB it asked for (600 MiB now)\n"
	if s := said(t, r); strings.Count(s, want) != 1 {
		t.Errorf("stderr %q, want once %q", s, want)
	}
	if hasTelemetryDir(st.Dir()) {
		t.Error("telemetry/ exists with telemetry off")
	}
}

// A stopped attempt comes back with the run's start need or its peak, never the trimmed need, under
// a new hold that the old request does not name: the new attempt is not trimmed. Mutation: keep
// the request found on the run across attempts → the new hold is trimmed, red.
func TestRestartAfterTrimIsUntrimmed(t *testing.T) {
	c, g, st := trimHold(t, 4000)
	r := trimRun(t, 2)
	r.o.need = 4000
	each := func(n int) {
		if n == 1 {
			requestTrim(t, c, g)
		}
	}
	bodies, p := trimWatch(t, r, g, st, []int64{200, 200, 200, 200}, each)
	if h := bodies[len(bodies)-1]; !h.Trimmed || h.NeedMiB != 600 {
		t.Fatalf("first attempt: trimmed %v need %d; want trimmed at 600", h.Trimmed, h.NeedMiB)
	}
	r.peakKiB = p.kib
	req := spec.Request{Label: "x", NeedMiB: 4000}
	r.requeue(&req, g)
	if req.NeedMiB != restartNeed(p.kib, 4000) || req.NeedMiB != 4000 {
		t.Errorf("restart need %d; want restartNeed(%d KiB, 4000) = 4000", req.NeedMiB, p.kib)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g2 := hold(t, c, "x", req.NeedMiB)
	bodies, _ = trimWatch(t, r, g2, st, []int64{200, 200, 200, 200}, nil)
	need, trimmed, asked := trimOf(bodies)
	if !slices.Equal(need, []int64{4000, 4000}) || !slices.Equal(trimmed, []bool{false, false}) || !slices.Equal(asked, []int64{0, 0}) {
		t.Errorf("second attempt: need %v trimmed %v asked %v; want 4000, neither key", need, trimmed, asked)
	}
}
