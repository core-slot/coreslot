//go:build coreslot_testhooks && unix

package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// The watchdog's traces step the machine readings of every run through the readings hook: the
// floor level comes from swap readings, and every run's own tree is measured for real.

// watchConfig is a budget of 64 GiB whose restarts wait for no calm, with extra appended to its
// [watch] table.
func watchConfig(extra string) string {
	return roomConfig(64<<10) + "[watch]\ncalm_s = 0\n" + extra
}

// floor is the readings of a host with no swap room left whose swap used grew to swapUsedMiB. The
// room counts MemAvailable on Linux, so the host's own would keep it above the floor.
func floor(swapUsedMiB int64) map[string]int64 {
	r := calm(swapUsedMiB)
	r["SwapFreeMiB"], r["VMVolumeFreeMiB"], r["MemAvailableMiB"] = 0, 0, 0
	return r
}

// Learn gives each label a counted run in the run log, so that the watchdog knows its normal S: a
// label with no history is never listed by a stop round.
func (e Env) Learn(labels ...string) {
	e.t.Helper()
	share := 0.1
	for _, l := range labels {
		if err := e.Engine().Store().AppendRun(store.RunRecord{TimeNS: store.NowNS(), Label: l, TickShare: &share}); err != nil {
			e.t.Fatal(err)
		}
	}
}

// WaitTicked waits until the run labelled label has written its tree into its hold, and returns
// the hold's name. A body read torn by the run's rewrite in place (PROTOCOL.md section 7.4) is read
// again.
func (e Env) WaitTicked(label string) string {
	e.t.Helper()
	var name string
	e.Eventually(startWait, label+" writes its tree into its hold", func() bool {
		h, ok := e.Hold(label)
		name = h.Name
		data, err := os.ReadFile(filepath.Join(e.Dir, store.HoldsDir, h.Name))
		if !ok || err != nil {
			return false
		}
		body, err := wire.DecodeHold(data)
		return err == nil && body.SampledNS > 0
	})
	return name
}

// Watch reads the shared detector.
func (e Env) Watch() watch.Detector {
	e.t.Helper()
	d, _, _, err := e.Engine().Store().ReadWatch()
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// WaitRound waits for a stop round after the one at sinceNS and returns the detector.
func (e Env) WaitRound(sinceNS int64) watch.Detector {
	e.t.Helper()
	var d watch.Detector
	e.Eventually(startWait, "a stop round", func() bool {
		d = e.Watch()
		return d.LastRoundNS != sinceNS
	})
	return d
}

// alive reports whether pid is a process that its parent has not reaped.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// A stop round kills the newest victim, enough to cover the deficit and at least one, and never
// the oldest live job: with room to spare, of A, B and C only C's command dies. Mutation: listing
// victims oldest first kills B; a run that kills whenever any round lists anything kills A and B.
func TestStopKillsNewestKeepsOldest(t *testing.T) {
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
	e.SetReadings(floor(1024))
	e.Eventually(startWait, "C's command is killed", func() bool { return !alive(kids[2].PID) })
	e.SetReadings(calm(1024))
	for i, name := range []string{"A", "B"} {
		if !alive(kids[i].PID) {
			t.Errorf("%s's command was killed; stderr:\n%s", name, runs[i].Stderr())
		}
	}
	for _, name := range []string{"A", "B", "C"} {
		e.ReleaseChild(name)
	}
	runs[0].ExitCode(0)
	runs[1].ExitCode(0)
}

// neverVictim starts A, then B with flags, puts the machine at the floor and checks that the stop
// round lists nothing and B's command lives on.
func neverVictim(t *testing.T, e Env, flags ...string) {
	t.Helper()
	a := e.Run("A", "--mem", "100M")
	e.WaitStarted("A")
	e.WaitTicked("A")
	b := e.Run("B", append([]string{"--mem", "100M"}, flags...)...)
	kid := e.WaitStarted("B")
	e.WaitTicked("B")
	e.SetReadings(floor(1024))
	d := e.WaitRound(0)
	e.SetReadings(calm(1024))
	if len(d.Stop) != 0 || d.Level != watch.Floor || !alive(kid.PID) {
		t.Errorf("round at %v lists %v; B's command alive %v; want a floor round listing nothing, B alive",
			d.Level, d.Stop, alive(kid.PID))
	}
	e.ReleaseChild("A")
	e.ReleaseChild("B")
	a.ExitCode(0)
	b.ExitCode(0)
}

// A command in coreslot's own process group is never a victim. Mutation: marking every run a
// victim lists B.
func TestSharedGroupNeverVictim(t *testing.T) {
	e := State(t)
	e.WriteConfig(watchConfig(""))
	neverVictim(t, e, "--same-group")
}

// A label with no completed run is a victim of the floor round, and its run asks for memory freed
// outside coreslot. Mutation: a floor round that lists learned jobs only leaves B's command alive.
func TestColdLabelStoppedAtTheFloor(t *testing.T) {
	e := State(t)
	e.WriteConfig(watchConfig(""))
	a := e.Run("A", "--mem", "100M")
	e.WaitStarted("A")
	e.WaitTicked("A")
	b := e.Run("B", "--mem", "100M")
	stopAgain(t, e, b, 1024)
	e.SetReadings(calm(2048))
	e.WaitStarted("B")
	e.ReleaseChild("A")
	e.ReleaseChild("B")
	a.ExitCode(0)
	b.ExitCode(0)
	if out := b.Stderr(); !strings.Contains(out, "free memory outside coreslot (close something)") {
		t.Errorf("stderr:\n%s\nwant the stop to ask for memory freed outside coreslot", out)
	}
}

// A label the needs file marks no_restart is never a victim. Mutation: ignoring the mark lists B.
func TestNoRestartLabelNeverVictim(t *testing.T) {
	e := State(t)
	e.WriteConfig(watchConfig(""))
	if err := os.WriteFile(filepath.Join(e.Dir, "needs.toml"), []byte("[needs.B]\nno_restart = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	neverVictim(t, e)
}

// A run declared --restart=no is never a victim, whatever its label. Mutation: deriving victim
// from no_restart alone lists B.
func TestRestartNoNeverVictim(t *testing.T) {
	e := State(t)
	e.WriteConfig(watchConfig(""))
	e.Learn("B")
	neverVictim(t, e, "--restart=no")
}

// A run writes its label's typical CPU time into its hold, from 10 counted runs that report it,
// for a red round's estimate; each tick also writes the CPU time so far. Mutation: not writing
// TypicalCPUS leaves typical_cpu_s at 0.
func TestHoldCarriesTheKeepEstimate(t *testing.T) {
	e := State(t)
	e.WriteConfig(watchConfig(""))
	user, sys, share := 3.0, 1.0, 0.1
	for range 10 {
		if err := e.Engine().Store().AppendRun(store.RunRecord{TimeNS: store.NowNS(), Label: "B", UserS: &user, SysS: &sys, TickShare: &share}); err != nil {
			t.Fatal(err)
		}
	}
	b := e.Run("B", "--mem", "100M")
	e.WaitStarted("B")
	name := e.WaitTicked("B")
	data, err := os.ReadFile(filepath.Join(e.Dir, store.HoldsDir, name))
	if err != nil {
		t.Fatal(err)
	}
	if h, err := wire.DecodeHold(data); err != nil || h.TypicalCPUS != 4 {
		t.Errorf("hold %+v (%v), want typical_cpu_s 4", h, err)
	}
	e.ReleaseChild("B")
	b.ExitCode(0)
}

// In observe mode a listed run says what it would stop and kills nothing. Mutation: killing in
// observe mode ends B's command.
func TestObserveKillsNothing(t *testing.T) {
	e := State(t)
	e.WriteConfig(watchConfig("mode = \"observe\"\n"))
	e.Learn("B")
	a := e.Run("A", "--mem", "100M")
	e.WaitStarted("A")
	e.WaitTicked("A")
	b := e.Run("B", "--mem", "100M")
	kid := e.WaitStarted("B")
	hold := e.WaitTicked("B")
	e.SetReadings(floor(1024))
	if d := e.WaitRound(0); len(d.Stop) != 1 || d.Stop[0] != hold {
		t.Fatalf("the round lists %v, want B's hold %s", d.Stop, hold)
	}
	e.Eventually(startWait, "B says it would stop", func() bool { return strings.Contains(b.Stderr(), "would stop B") })
	e.SetReadings(calm(1024))
	if !alive(kid.PID) {
		t.Errorf("B's command was killed in observe mode; stderr:\n%s", b.Stderr())
	}
	e.ReleaseChild("A")
	e.ReleaseChild("B")
	a.ExitCode(0)
	b.ExitCode(0)
}

// A label's normal share is the median of its earlier runs' tick shares: a run starting on them
// carries it in its hold, and one whose label has runs with only user_s and sys_s is learning.
// Mutation: learn from user_s and sys_s, and the second label is learned.
func TestNormalShareComesFromTickShares(t *testing.T) {
	e := State(t)
	e.WriteConfig(watchConfig(""))
	log := e.Engine().Store()
	user, sys := 0.5, 0.5
	for i, share := range []float64{0.1, 0.5, 0.2} {
		if err := log.AppendRun(store.RunRecord{TimeNS: store.NowNS() + int64(i), Label: "ticked", TickShare: &share}); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.AppendRun(store.RunRecord{TimeNS: store.NowNS(), Label: "timed", UserS: &user, SysS: &sys}); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"ticked", "timed"} {
		e.Run(label, "--mem", "100M")
		e.WaitStarted(label)
	}
	ticked, timed := e.HoldBody(e.WaitTicked("ticked")), e.HoldBody(e.WaitTicked("timed"))
	if !ticked.Learned || ticked.NormalShare != 0.2 || timed.Learned {
		t.Errorf("holds: ticked learned %v at %v, timed learned %v; want 0.2 learned, and timed learning",
			ticked.Learned, ticked.NormalShare, timed.Learned)
	}
	e.ReleaseChild("ticked")
	e.ReleaseChild("timed")
}

// A real run writes its tick share to the run log, and a later run of the label learns it as its
// normal share. Mutation: not writing rec.TickShare in the run leaves the label learning.
func TestRunLearnsItsTickShareForTheNextRun(t *testing.T) {
	e := State(t)
	e.WriteConfig(watchConfig(""))
	e.SetReadings(calm(0))
	first := e.Run("learner", "--mem", "100M")
	e.WaitStarted("learner")
	name := e.WaitTicked("learner")
	// The idle hold child can spend under one 10 ms /proc tick in a period, so compared samples
	// alone do not give the run a gate-closed tick that had time. Wait for one: the hold's last
	// tick had time, and the detector that tick returned (sampled after the hold was written,
	// which did not change meanwhile) is green with its gate closed.
	hold := func() (wire.Hold, bool) {
		data, err := os.ReadFile(filepath.Join(e.Dir, store.HoldsDir, name))
		h, derr := wire.DecodeHold(data) // the run rewrites its hold in place: a read can be torn
		return h, err == nil && derr == nil
	}
	e.Eventually(startWait, "a gate-closed tick that had time", func() bool {
		h, ok := hold()
		d, _, at, err := e.Engine().Store().ReadWatch()
		again, ok2 := hold()
		return ok && ok2 && err == nil && again.SampledNS == h.SampledNS && h.SampledNS > 0 &&
			h.DUserNS+h.DSysNS+h.DPageWaitNS > 0 && at >= h.SampledNS && len(d.Window) > 0 && !d.Gate &&
			d.Level == watch.Green
	})
	e.ReleaseChild("learner")
	first.ExitCode(0)
	runs, _, err := e.Engine().Store().ReadRuns()
	if err != nil || len(runs) != 1 || runs[0].TickShare == nil {
		t.Fatalf("run log %+v, %v; want one run with a tick_share", runs, err)
	}
	e.Start("run", "--restart=yes", "--label", "learner", "--mem", "100M", "--", bins.child, "hold", e.MarkDir, "next")
	e.WaitStarted("next")
	h := e.HoldBody(e.WaitTicked("learner"))
	if !h.Learned || h.NormalShare != *runs[0].TickShare {
		t.Errorf("second run learned %v at %v; want the first run's %v", h.Learned, h.NormalShare, *runs[0].TickShare)
	}
	e.ReleaseChild("next")
}
