//go:build unix

package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// setLiveMem replaces the headroom testMachine reports, in MiB, for one test.
func setLiveMem(t *testing.T, headroomMiB int64) {
	t.Helper()
	saved := testHeadroom
	t.Cleanup(func() { testHeadroom = saved })
	testHeadroom.mib, testHeadroom.ok = headroomMiB, true
}

// The room is the headroom less the reserve and every live hold's unmeasured need: of 4096 MiB,
// 1024 are the reserve and A's 1 GiB, so B's 2 GiB fits exactly; C's does not. Mutation: forgetting
// the reserve admits C; comparing need < room refuses B.
func TestRoomGatesAdmission(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	setLiveMem(t, 4096)
	admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	admitted(t, step(t, enqueue(t, e, need("mem=2G"))))
	if r := step(t, enqueue(t, e, need("mem=1M"))); isAdmitted(r) {
		t.Fatal("a request admitted with no room left")
	}
}

// With no hold live the head starts whatever the numbers say; the next request, with a hold live,
// is held to them. Mutation: gating on the room when idle refuses the first.
func TestIdleHeadAlwaysStarts(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	setLiveMem(t, 100) // below the reserve
	g := admitted(t, step(t, enqueue(t, e, need("mem=4G"))))
	if g.GrantMiB() != 4096 {
		t.Errorf("grant %d, want the 4096 asked", g.GrantMiB())
	}
	if r := step(t, enqueue(t, e, need("mem=1M"))); isAdmitted(r) {
		t.Fatal("a request admitted beside a live hold with the room negative")
	}
}

// A headroom that cannot be read gates nothing, without an error, and lets nobody pass a ticket
// ahead. Mutation: reading the headroom as 0 when it is not ok, and B waits.
func TestUnreadHeadroomGatesNothing(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	setLiveMem(t, 0)
	testHeadroom.ok = false
	admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	admitted(t, step(t, enqueue(t, e, need("mem=2G"))))
}

// A hold hands the room back by the footprint its run wrote: with 3584 MiB, A's 1 GiB with no
// footprint leaves 1536 MiB after the reserve, too little for B's 2 GiB. With A's footprint at
// 1 GiB nothing of it is still to grow, 2560 MiB are free and B is admitted. Mutation: ignoring
// the footprint keeps B out.
func TestRoomCountsFootprint(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	setLiveMem(t, 3584)
	a := admitted(t, step(t, enqueue(t, e, need("mem=1G"))))
	b := enqueue(t, e, need("mem=2G"))
	if isAdmitted(step(t, b)) {
		t.Fatal("B admitted beside A's whole need")
	}
	if err := a.UpdateHold(func(h *wire.Hold) { h.FootprintMiB = 1024 }); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Second) // the head's next attempt
	admitted(t, step(t, b))
}

// A live hold whose body does not parse (its run rewrites it in place, so a read can be torn)
// counts at the cold start of the machine, 426 MiB of 1707, not at zero: 2048 MiB of headroom less
// the 1024 reserve and 426 leaves 598. Mutation: counting it at zero admits 600.
func TestTornLiveHoldCountsAtColdStart(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	setLiveMem(t, 2048)
	if err := os.MkdirAll(filepath.Join(dir, store.HoldsDir), 0o700); err != nil {
		t.Fatal(err)
	}
	torn := filepath.Join(dir, store.HoldsDir, wire.HoldName(1, 4242, 1, "torn"))
	if err := os.WriteFile(torn, []byte(`{"label":"torn","pid":42`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(torn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	over := enqueue(t, e, need("mem=600M"))
	if r := step(t, over); isAdmitted(r) {
		t.Fatal("600 MiB admitted with the torn hold counted at zero")
	}
	if err := over.Withdraw(); err != nil {
		t.Fatal(err)
	}
	admitted(t, step(t, enqueue(t, e, need("mem=598M"))))
}
