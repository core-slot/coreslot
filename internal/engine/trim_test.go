//go:build unix

package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// holdID is the id of the grant's hold: what trim.json names.
func holdID(t *testing.T, g *Grant) wire.ID {
	t.Helper()
	id, ok := wire.ParseName(g.HoldName())
	if !ok {
		t.Fatalf("hold name %q does not parse", g.HoldName())
	}
	return id
}

func trimPath(dir string) string { return filepath.Join(dir, store.TrimFile) }

// Each refusal names a request that could do nothing, and none writes trim.json; none is a state
// error. Mutations: publishing before the checks writes the file in every case; leaving the new
// sentinels out of StateReason makes each a state error.
func TestTrimRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string, e *Engine) wire.ID
		want  error
	}{
		{"no hold", func(t *testing.T, dir string, e *Engine) wire.ID {
			return wire.ID{OrderKey: 1, PID: 4711, Seq: 1}
		}, ErrNoHold},
		{"a dead hold", func(t *testing.T, dir string, e *Engine) wire.ID {
			id, _ := wire.ParseName(filepath.Base(holdPath(t, dir, 512)))
			return id
		}, ErrNoHold},
		{"need 0", func(t *testing.T, dir string, e *Engine) wire.ID {
			g, err := e.FailOpen(need(256), "test")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { g.Close() })
			return holdID(t, g)
		}, ErrNothingReserved},
		{"child exited", func(t *testing.T, dir string, e *Engine) wire.ID {
			g := hold(t, e, need(256))
			if err := g.UpdateHold(func(h *wire.Hold) { h.ChildExited = true }); err != nil {
				t.Fatal(err)
			}
			return holdID(t, g)
		}, ErrCommandExited},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := newStateDir(t, "")
			e := newEngine(t, dir, newFakeClock())
			id := c.setup(t, dir, e)
			err := e.TrimContext(context.Background(), id)
			if !errors.Is(err, c.want) {
				t.Fatalf("TrimContext = %v; want %v", err, c.want)
			}
			if _, isState := StateReason(err); isState {
				t.Errorf("%v is classed as a state error", err)
			}
			if exists(trimPath(dir)) {
				t.Error("a refused trim wrote trim.json")
			}
		})
	}
}

// In a state directory of another format a trim refuses and writes nothing at all. Mutation:
// skipping the understood check after Prepare writes trim.json into it.
func TestTrimForeignFormatWritesNothing(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	g := hold(t, e, need(256))
	writeFormat(t, dir, "coreslot-protocol 4\n")
	before := listing(t, dir)
	err := e.TrimContext(context.Background(), holdID(t, g))
	var fe *store.FormatError
	if !errors.Is(err, ErrRefused) || !errors.As(err, &fe) || fe.N != 4 {
		t.Fatalf("TrimContext = %v; want a refusal naming format 4", err)
	}
	if after := listing(t, dir); after != before {
		t.Errorf("a refused trim wrote to the state directory:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A trim of a live hold writes its request, keeps the request of a hold that holds/ still lists
// though dead, drops one whose hold is gone, and bumps no gen.
// Mutations: passing only the live holds as listed drops the dead hold's request; bumping gen
// after the write changes gen.
func TestTrimPublishesForLiveHold(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	g := hold(t, e, need(256)) // its section reconciled, so the dead hold below stays listed
	deadID, _ := wire.ParseName(filepath.Base(holdPath(t, dir, 512)))
	goneID := wire.ID{OrderKey: 2, PID: 4711, Seq: 1}
	body := fmt.Sprintf(`{"holds":{%q:{},%q:{}}}`, deadID, goneID)
	if err := os.WriteFile(trimPath(dir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	gen := readGen(t, e)
	if err := e.TrimContext(context.Background(), holdID(t, g)); err != nil {
		t.Fatal(err)
	}
	got, err := e.st.ReadTrim()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{deadID.String(): true, holdID(t, g).String(): true}
	if !maps.Equal(got, want) {
		t.Errorf("trim.json holds %v; want %v", got, want)
	}
	if after := readGen(t, e); after != gen {
		t.Errorf("gen %d -> %d; a trim bumps no gen", gen, after)
	}
}

// A live hold whose body reads torn is found by its name and not refused: what it reserves is not
// known, which is not zero. Mutation: listing a torn body as reserving nothing refuses it with
// ErrNothingReserved.
func TestTrimTornBodyByID(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	g := hold(t, e, need(256))
	if err := os.WriteFile(filepath.Join(dir, store.HoldsDir, g.HoldName()), []byte(`{"label":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.TrimContext(context.Background(), holdID(t, g)); err != nil {
		t.Fatalf("TrimContext of a torn body = %v; want the request written", err)
	}
	if got, _ := e.st.ReadTrim(); len(got) != 1 {
		t.Errorf("trim.json holds %v; want the torn hold's request", got)
	}
}

// A grant finds a request for its own hold and no other's: two holds of one label and one pid.
// Mutation: matching the request by the id's pid alone (or by the label) makes the second grant
// find the first one's request.
func TestGrantTrimRequested(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	g := hold(t, e, needLabel(256, "same"))
	other := hold(t, e, needLabel(256, "same"))
	if g.TrimRequested() {
		t.Fatal("a request found before any trim")
	}
	if err := e.TrimContext(context.Background(), holdID(t, g)); err != nil {
		t.Fatal(err)
	}
	if !g.TrimRequested() {
		t.Error("TrimRequested = false; want the request just written")
	}
	if other.TrimRequested() {
		t.Error("the other hold found a request; want none")
	}
	if err := os.WriteFile(trimPath(dir), []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if g.TrimRequested() {
		t.Error("a request found in a malformed trim.json")
	}
}

// A snapshot's hold carries its id, the need it asked for and its trimmed mark. Mutation:
// leaving AskedMiB or Trimmed out of info() reads them as zero.
func TestHolderInfoCarriesAskedAndTrimmed(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	g := hold(t, e, need(256))
	if err := g.UpdateHold(func(h *wire.Hold) { h.NeedMiB, h.AskedMiB, h.Trimmed = 100, 256, true }); err != nil {
		t.Fatal(err)
	}
	r, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Holds) != 1 {
		t.Fatalf("holds %+v; want one", r.Holds)
	}
	h := r.Holds[0]
	if h.ID != holdID(t, g).String() || h.NeedMiB != 100 || h.AskedMiB != 256 || !h.Trimmed {
		t.Errorf("hold %+v; want id %s, need 100, asked 256, trimmed", h, holdID(t, g))
	}
}
