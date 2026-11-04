//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

// A run acts on the round that listed it as that round was written: a floor round's stop asks for
// memory outside coreslot even when the level has turned red or unknown by the run's tick, and a
// round written in observe mode kills nothing after a switch to enforce. Mutations: keying the
// stop to det.Level says "memory red" and asks nothing; keying it to the current mode kills.
func TestStopFollowsTheRound(t *testing.T) {
	for _, c := range []struct {
		name               string
		det                watch.Detector
		killed, floor      bool
		wantStop, wantSaid string
	}{
		{"floor round, red at the tick", watch.Detector{Level: watch.Red, RoundFloor: true}, true, true, "swap room under the floor, at 1 GiB", ""},
		{"floor round, unknown at the tick", watch.Detector{Level: watch.Unknown, RoundFloor: true}, true, true, "swap room under the floor, at 1 GiB", ""},
		{"red round, floor at the tick", watch.Detector{Level: watch.Floor}, true, false, "memory red, at 1 GiB", ""},
		{"observe round under enforce", watch.Detector{Level: watch.Red, Observed: true}, false, false, "", "would stop x (memory red, at 1 GiB)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			r := &run{o: runOptions{label: "x"}, io: IO{Stderr: stderr}}
			kills := 0
			stopped := r.stopFor(c.det, 1<<20, memsample.TreeTimes{}, false, func() bool { kills++; return true })
			if stopped != c.killed || kills != map[bool]int{true: 1}[c.killed] || r.lastFloor != c.floor || r.lastStop != c.wantStop {
				t.Errorf("stopped %v after %d kills, floor %v, stop %q; want %v, floor %v, stop %q",
					stopped, kills, r.lastFloor, r.lastStop, c.killed, c.floor, c.wantStop)
			}
			if said, _ := os.ReadFile(stderr.Name()); !strings.Contains(string(said), c.wantSaid) {
				t.Errorf("stderr %q, want %q", said, c.wantSaid)
			}
		})
	}
}

// A kept command says so once per round, with its progress, its estimate and its deadline, as
// the stop lines do; a stop of a command an earlier round kept says that too. Mutations: dropping
// wasKept from stopFor leaves the stop line without it; not remembering the round says it twice.
func TestKeptLines(t *testing.T) {
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	r := &run{o: runOptions{label: "x", argv: []string{"go", "test", "./..."}}, io: IO{Stderr: stderr}}
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)
	r.sayKept(true, watch.Kept{DeadlineNS: 30e9, LeftS: 9.4, Done: 0.8, RoundNS: 20e9}, at)
	det := watch.Detector{LastRoundNS: 20e9, Kept: map[string]watch.Kept{"h": {DeadlineNS: 30e9, LeftS: 9.4, Done: 0.8, RoundNS: 20e9}}}
	last := r.keptNotice(det, "h", 0)
	if again := r.keptNotice(det, "h", last); last != 20e9 || again != last {
		t.Errorf("rounds said for %d then %d, want 20e9 both", last, again)
	}
	r.keptNotice(watch.Detector{LastRoundNS: 40e9, Kept: det.Kept}, "h", last) // a later round that did not keep it
	r.stopFor(watch.Detector{Level: watch.Red}, 1<<20, memsample.TreeTimes{}, true, func() bool { return true })
	said, _ := os.ReadFile(stderr.Name())
	if want := "coreslot: would keep x (memory red, about 9 s left, 80% done, until 12:00:10)\n"; !strings.Contains(string(said), want) {
		t.Errorf("stderr %q, want %q", said, want)
	}
	if n := strings.Count(string(said), "coreslot: kept `go test ./...` running (memory red, about 9 s left, 80% done, until "); n != 1 {
		t.Errorf("stderr %q says the keep %d times, want once", said, n)
	}
	if want := "memory red, at 1 GiB; an earlier round kept it as nearly finished, this one did not"; r.lastStop != want {
		t.Errorf("stop %q, want %q", r.lastStop, want)
	}
}
