//go:build coreslot_testhooks && unix

package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// Two jobs run; a quiet job waits for room with two more behind it. When a running job ends the
// quiet job starts in its turn, the two behind it wait until it ends though the room would take
// them, and the job still running goes on. Mutation: dropping step 1c starts them beside it.
func TestTraceQuietJobHoldsTheQueue(t *testing.T) {
	e := State(t)
	e.WriteConfig(roomConfig(5120))
	a, b := e.Run("A", "--mem", "2G"), e.Run("B", "--mem", "2G")
	e.WaitStarted("A")
	e.WaitStarted("B")
	q := e.Run("Q", "--mem", "2G")
	e.Eventually(startWait, "Q says it waits", func() bool { return strings.Contains(q.Stderr(), "waiting for") })
	mark := e.Start("quiet", e.WaitTicket("Q").ID)
	mark.ExitCode(0)
	x, y := e.Run("X", "--mem", "512M"), e.Run("Y", "--mem", "512M")
	e.WaitTicket("X")
	e.WaitTicket("Y")

	e.ReleaseChild("A")
	a.ExitCode(0)
	e.WaitStarted("Q")
	if h, ok := e.Hold("Q"); !ok || !h.Quiet {
		t.Fatalf("Q's hold %+v (live %v); want it marked quiet", h, ok)
	}
	time.Sleep(3 * time.Second) // the waiters retry every second at most
	for _, name := range []string{"X", "Y"} {
		if _, ok := e.Started(name); ok {
			t.Errorf("%s started beside the quiet job", name)
		}
	}
	if _, ok := e.Hold("B"); !ok {
		t.Error("B, already running, did not go on")
	}
	e.ReleaseChild("Q")
	q.ExitCode(0)
	e.WaitStarted("X")
	e.WaitStarted("Y")
	for _, name := range []string{"B", "X", "Y"} {
		e.ReleaseChild(name)
	}
	for _, p := range []*Proc{b, x, y} {
		p.ExitCode(0)
	}
}

// A floor round with the newest job marked do-not-stop stops the next newest instead, and never
// the marked one. Mutation: writing victim from the flags alone kills C.
func TestTraceNoStopJobIsPassedOver(t *testing.T) {
	e := State(t)
	e.WriteConfig(watchConfig(""))
	e.Learn("A", "B", "C")
	var runs []*Proc
	var kids []ChildReport
	for _, name := range []string{"A", "B", "C"} {
		runs = append(runs, e.Run(name, "--mem", "100M"))
		kids = append(kids, e.WaitStarted(name))
		e.WaitTicked(name)
	}
	c, _ := e.Hold("C")
	mark := e.Start("no-stop", c.ID)
	mark.ExitCode(0)
	marked := store.NowNS()
	e.Eventually(startWait, "C's run writes its hold after reading the mark", func() bool {
		data, err := os.ReadFile(filepath.Join(e.Dir, store.HoldsDir, c.Name))
		h, derr := wire.DecodeHold(data) // a body torn by the run's rewrite in place is read again
		return err == nil && derr == nil && h.SampledNS > marked+int64(100*time.Millisecond)
	})
	e.SetReadings(floor(1024))
	e.Eventually(startWait, "B's command is killed", func() bool { return !alive(kids[1].PID) })
	e.SetReadings(calm(1024))
	if !alive(kids[2].PID) {
		t.Errorf("C's command was killed though marked do-not-stop; stderr:\n%s", runs[2].Stderr())
	}
	for _, name := range []string{"A", "B", "C"} {
		e.ReleaseChild(name)
	}
	runs[0].ExitCode(0)
	runs[2].ExitCode(0)
}
