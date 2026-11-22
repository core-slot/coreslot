//go:build coreslot_testhooks && unix

package conformance

import (
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/store"
)

// yieldScene teaches the run log a long label, "long", of 40 min and short labels of the given run
// times, sets the kernel's pressure to normal through the readings hook, and starts "big", which
// holds the whole 4 GiB room until released, and the long head of 3 GiB behind it.
func yieldScene(t *testing.T, shorts map[string]float64) (Env, *Proc, *Proc) {
	e := State(t)
	e.WriteConfig(roomConfig(4096))
	runs := map[string]float64{"long": 2400}
	for l, s := range shorts {
		runs[l] = s
	}
	for l, s := range runs {
		if err := e.Engine().Store().AppendRun(store.RunRecord{TimeNS: store.NowNS(), Label: l, RanS: s}); err != nil {
			t.Fatal(err)
		}
	}
	e.SetReadings(map[string]int64{"PressureLevel": 1})
	big := e.Run("big", "--mem", "4G")
	e.WaitStarted("big")
	head := e.Run("long", "--mem", "3G")
	e.Eventually(startWait, "the head says it waits", func() bool { return strings.Contains(head.Stderr(), "waiting for") })
	return e, big, head
}

// A long head that fits once the room frees yields to the two short jobs behind it, which it would
// displace: both start side by side first, and the head starts once they end, within its deadline.
// Mutation: a head that never yields starts first and keeps both short jobs out.
func TestTraceLongHeadYieldsToShortJobs(t *testing.T) {
	e, big, head := yieldScene(t, map[string]float64{"s1": 30, "s2": 30})
	s1, s2 := e.Run("s1", "--mem", "2G"), e.Run("s2", "--mem", "2G")
	for _, p := range []*Proc{s1, s2} {
		e.Eventually(startWait, p.name+" says it waits", func() bool { return strings.Contains(p.Stderr(), "waiting for") })
	}
	e.ReleaseChild("big")
	big.ExitCode(0)
	e.WaitStarted("s1")
	e.WaitStarted("s2")
	if _, ok := e.Started("long"); ok {
		t.Fatal("the head started beside the short jobs")
	}
	e.ReleaseChild("s1")
	e.ReleaseChild("s2")
	s1.ExitCode(0)
	s2.ExitCode(0)
	e.WaitStarted("long")
	e.ReleaseChild("long")
	head.ExitCode(0)
}

// A short job that runs past its typical run time ends the yield: the second short job does not
// pass, and the head starts when the first ends. Mutation: a head that ignores the overrun keeps
// its yield, and with it the record.
func TestTraceAnOverrunEndsTheYield(t *testing.T) {
	e, big, head := yieldScene(t, map[string]float64{"s1": 1, "s2": 30})
	s1, s2 := e.Run("s1", "--mem", "2G"), e.Run("s2", "--mem", "2560M") // s2 does not fit beside s1
	for _, p := range []*Proc{s1, s2} {
		e.Eventually(startWait, p.name+" says it waits", func() bool { return strings.Contains(p.Stderr(), "waiting for") })
	}
	e.ReleaseChild("big")
	big.ExitCode(0)
	e.WaitStarted("s1")
	e.Eventually(startWait, "the head sees the overrun", func() bool {
		_, ok, err := e.Engine().Store().ReadYield()
		return err == nil && !ok
	})
	e.ReleaseChild("s1")
	s1.ExitCode(0)
	e.WaitStarted("long")
	if _, ok := e.Started("s2"); ok {
		t.Error("the second short job started past the head after the overrun")
	}
	e.ReleaseChild("long")
	head.ExitCode(0)
	e.WaitStarted("s2")
	e.ReleaseChild("s2")
	s2.ExitCode(0)
}
