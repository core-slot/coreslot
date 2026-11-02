//go:build unix

package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
)

// The supervisor kills the command's group while the command is unreaped, and never after:
// its pid may then name another process's group. Mutation: dropping the reaped check in
// KillGroup signals the reaped group.
func TestKillAfterReapIsRefused(t *testing.T) {
	var sent []int
	saved := killGroup
	t.Cleanup(func() { killGroup = saved })
	killGroup = func(pgid int) error {
		sent = append(sent, pgid)
		return saved(pgid)
	}
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sup := newSupervisor(cmd, true)
	if !sup.KillGroup() {
		t.Fatal("KillGroup of the running command refused")
	}
	if code := superviseChild(cmd, sup, false); code != 128+int(syscall.SIGKILL) {
		t.Fatalf("status %d, want %d", code, 128+int(syscall.SIGKILL))
	}
	if sup.KillGroup() {
		t.Error("KillGroup after the reap reported a kill")
	}
	if len(sent) != 1 || sent[0] != cmd.Process.Pid {
		t.Errorf("signalled groups %v, want only %d, before the reap", sent, cmd.Process.Pid)
	}
}

// A run's tick share is the share of the time it summed over the ticks whose sample had the paging
// gate closed: a tick whose gate was open, whose sample could not be read, or that took no sample
// adds nothing, and a run with no such tick has no share.
// Mutation: count every tick, and the share is 0.85.
func TestTickShareCountsClosedGateOnly(t *testing.T) {
	sample := []watch.Point{{AtNS: 1}}
	closed, open := watch.Detector{Window: sample}, watch.Detector{Window: sample, Gate: true}
	var r run
	r.countClosed(memsample.TreeTimes{UserNS: 3e9, SysNS: 1e9}, closed, nil)
	r.countClosed(memsample.TreeTimes{SysNS: 9e9, PageWaitNS: 9e9}, open, nil)
	r.countClosed(memsample.TreeTimes{SysNS: 9e9}, closed, errors.New("sample not read"))
	r.countClosed(memsample.TreeTimes{SysNS: 9e9}, watch.Detector{}, nil)
	if got := r.tickShare(); got == nil || *got != 0.25 {
		t.Errorf("tick share %v; want 0.25", got)
	}
	var none run
	none.countClosed(memsample.TreeTimes{UserNS: 1e9}, open, nil)
	none.countClosed(memsample.TreeTimes{}, closed, nil)
	if got := none.tickShare(); got != nil {
		t.Errorf("tick share %v with no closed tick that had time; want none", *got)
	}
}

// A tick counts only when its sample's pair was compared and the gate was closed: the first
// sample and one across a gap have no gate reading, whatever the paging. Mutation: testing only
// the window and the gate counts all three samples.
func TestTickShareIgnoresSamplesWithoutAGateReading(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	sec, err := st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	var r run
	now := store.NowNS()
	for _, at := range []int64{0, 2e9, 12e9} { // the first sample, a compared one, one 10 s on
		read := func() (watch.Sample, error) {
			var s watch.Sample
			s.Host.WallNS, s.Host.AwakeNS = 1e12+at, 1e12+at
			return s, nil
		}
		det, _, err := sec.Tick(now+at, read, watch.Defaults())
		r.countClosed(memsample.TreeTimes{SysNS: 1e9, UserNS: 1e9}, det, err)
	}
	if r.closed.DSysNS != 1e9 {
		t.Errorf("counted %d ns of system time; want only the compared sample's 1 s", r.closed.DSysNS)
	}
}
