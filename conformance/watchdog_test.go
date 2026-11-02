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
	user, sys := 0.9, 0.1
	for _, l := range labels {
		if err := e.Engine().Store().AppendRun(store.RunRecord{TimeNS: store.NowNS(), Label: l, UserS: &user, SysS: &sys}); err != nil {
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

// A label with no completed run is never a victim, even at the floor. Mutation: listing it
// anyway lists B.
func TestColdLabelNeverVictim(t *testing.T) {
	e := State(t)
	e.WriteConfig(watchConfig(""))
	neverVictim(t, e)
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
