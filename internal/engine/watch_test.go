//go:build unix

package engine

import (
	"context"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/watch"
)

// sampleWallNS is the sample clock of the watch states the tests write.
const sampleWallNS = int64(1e12)

// setWatch stores d as the watch state of a sample the engine's clock took age ago.
func setWatch(t *testing.T, e *Engine, clk *fakeClock, d watch.Detector, age time.Duration) {
	t.Helper()
	sec, err := e.st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	var s watch.Sample
	s.Host.WallNS = sampleWallNS
	if err := sec.SetWatch(d, s, clk.clock().Realtime()-int64(age)); err != nil {
		t.Fatal(err)
	}
}

// A yellow sample 1 s old holds admission; one 10 s old is no reading and holds nothing.
// Mutation: dropping the age check keeps the second waiter out.
func TestStaleLevelIgnored(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	hold(t, e, need("mem=1"))
	setWatch(t, e, clk, watch.Detector{Level: watch.Yellow}, time.Second)
	tk := enqueue(t, e, need("mem=1"))
	if isAdmitted(step(t, tk)) {
		t.Fatal("admitted while a fresh sample is yellow")
	}
	setWatch(t, e, clk, watch.Detector{Level: watch.Yellow}, 10*time.Second)
	clk.advance(time.Second) // the head's next attempt
	admitted(t, step(t, tk))
}

// With no hold live the head starts whatever the level says. Mutation: reading the level before
// the idle rule keeps it out.
func TestIdleIgnoresLevel(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	setWatch(t, e, clk, watch.Detector{Level: watch.Red}, 0)
	admitted(t, step(t, enqueue(t, e, need("mem=1"))))
}

// After the level clears, one job starts per spacing_s.
func TestSpacingAfterClear(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	hold(t, e, need("mem=1"))
	setWatch(t, e, clk, watch.Detector{YellowClearedNS: sampleWallNS - 1e9}, 0)
	admitted(t, step(t, enqueue(t, e, need("mem=1"))))
	tk := enqueue(t, e, need("mem=1"))
	if isAdmitted(step(t, tk)) {
		t.Fatal("a second job started within spacing_s of the first after the clear")
	}
	clk.advance(3 * time.Second)
	admitted(t, step(t, tk))
}

// A restarted job waits for calm_s of green and for room of its need plus max(256 MiB, need/10),
// where a new job of the same need starts. The room beside the 1-unit hold is 896 MiB.
func TestRestartWaitsForCalmAndMargin(t *testing.T) {
	clk := newFakeClock()
	e := newEngine(t, newStateDir(t, ""), clk)
	hold(t, e, need("mem=1"))
	restart := func(mib int64) bool {
		t.Helper()
		req := need("mem=1")
		req.NeedMiB, req.Restart = mib, true
		tk := enqueue(t, e, req)
		ok := isAdmitted(step(t, tk))
		if !ok {
			_ = tk.Withdraw() // out of the way of the next request
		}
		return ok
	}
	setWatch(t, e, clk, watch.Detector{CalmSinceNS: sampleWallNS - 10e9}, 0)
	if restart(640) {
		t.Fatal("a restart started after 10 s of calm")
	}
	setWatch(t, e, clk, watch.Detector{CalmSinceNS: sampleWallNS - 31e9}, 0)
	if restart(768) {
		t.Fatal("a 768 MiB restart started with 896 MiB of room, under its 256 MiB margin")
	}
	admitted(t, step(t, enqueue(t, e, need("mem=768M")))).Close()
	if !restart(640) {
		t.Error("a 640 MiB restart did not start after 31 s of calm with room for its margin")
	}
}
