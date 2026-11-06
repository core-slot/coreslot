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

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
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

// watchedHold publishes a real hold for the tests of a run's marking.
func watchedHold(t *testing.T) (*engine.Grant, *store.Store) {
	t.Helper()
	c := openLib(t, stateDir(t, ""))
	return hold(t, c, "x", 1), c.Store()
}

// body is g's hold as a reader finds it.
func body(t *testing.T, st *store.Store, g *engine.Grant) wire.Hold {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(st.Dir(), store.HoldsDir, g.HoldName()))
	if err != nil {
		t.Fatal(err)
	}
	h, err := wire.DecodeHold(b)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// watchFor runs r's watchChild for the given number of 1 s listings, an even one's tick cut off
// by the end. Listing n reads n MiB, so each is a new peak; each, when set, runs at its start,
// and client answers its scan.
func watchFor(t *testing.T, r *run, sup *supervisor, g *engine.Grant, st *store.Store, listings int,
	client func(n int) bool, each func(n int)) (bool, peak) {
	t.Helper()
	savedTree, savedClient := listTree, listClient
	t.Cleanup(func() { listTree, listClient = savedTree, savedClient })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	listTree = func(int) (int64, map[int]memsample.TreeTimes, bool) {
		n++
		if each != nil {
			each(n)
		}
		if n == listings {
			cancel()
		}
		return int64(n) << 10, nil, true
	}
	listClient = func(int) bool { return client(n) }
	return r.watchChild(ctx, sup, g, st)
}

// restartable is a run a stop round may pick, as the CLI starts one.
func restartable(argv ...string) *run {
	o := runOptions{label: "x", argv: argv, restart: "yes"}
	o.resolveNeed(nil, noLearned, 16<<10)
	return &run{o: o}
}

// A docker command is container work from its first hold update, whatever --restart says; group is
// what makes the Victim false mean something. Mutations: leave the Container write out of the
// first update → false, red; leave containerCommand out of resolveNeed's noRestart → a victim, red.
func TestProgramRuleHoldStartsContainer(t *testing.T) {
	g, st := watchedHold(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, p := restartable("/usr/local/bin/docker", "run", "x").watchChild(ctx, &supervisor{pid: 42, group: true}, g, st)
	if h := body(t, st, g); h.Victim || !h.Container || !p.container {
		t.Errorf("hold Victim %v Container %v, peak %v; want false, true, true", h.Victim, h.Container, p.container)
	}
}

// A run's tree finds a client: the next tick's write clears Victim and sets the mark, and the scan
// is done. Mutation: set Victim true on the hit → still a victim, red; scan on after the hit → 4
// scans, red.
func TestTreeHitClearsVictim(t *testing.T) {
	g, st := watchedHold(t)
	scans := 0
	client := func(n int) bool { scans++; return n >= 3 }
	each := func(n int) {
		if h := body(t, st, g); n > 1 && n <= 3 && (!h.Victim || h.Container) {
			t.Errorf("listing %d, before the hit: Victim %v Container %v; want true, false", n, h.Victim, h.Container)
		}
	}
	_, p := watchFor(t, restartable("make", "test"), &supervisor{pid: 42, group: true}, g, st, 4, client, each)
	if h := body(t, st, g); h.Victim || !h.Container || !p.container {
		t.Errorf("after the hit: Victim %v Container %v, peak %v; want false, true, true", h.Victim, h.Container, p.container)
	}
	if scans != 3 {
		t.Errorf("%d scans; want 3, none after the hit", scans)
	}
}

// The client may close while its containers run on: the mark stays on the hold, through the ticks
// that rewrite it, and on the peak, through a later, higher reading. Mutation: keep rebuilding the
// peak on a higher reading → the mark is gone, red.
func TestHitIsSticky(t *testing.T) {
	g, st := watchedHold(t)
	client := func(n int) bool { return n == 1 }
	_, p := watchFor(t, restartable("make", "test"), &supervisor{pid: 42, group: true}, g, st, 3, client, nil)
	if h := body(t, st, g); !h.Container || h.Victim || h.SampledNS == 0 {
		t.Errorf("hold Container %v Victim %v, sampled %d; want true, false after a tick", h.Container, h.Victim, h.SampledNS)
	}
	if !p.container || p.kib != 3<<10 {
		t.Errorf("peak %+v; want the mark kept at the third listing's 3 MiB", p)
	}
}

// A stop list that named the hold before the hit stops nothing once the hit is known: a stop
// would kill the client and leave its container. Mutation: drop p.container from the stop check →
// the command is killed, red.
func TestHitIgnoresAStoredStop(t *testing.T) {
	g, st := watchedHold(t)
	sec, err := st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := sec.SetWatch(watch.Detector{Stop: []string{g.HoldName()}, LastRoundNS: 1}, watch.Sample{}, store.NowNS()); err != nil {
		t.Fatal(err)
	}
	sec.Leave()
	kills := 0
	saved := killGroup
	t.Cleanup(func() { killGroup = saved })
	killGroup = func(int) error { kills++; return nil }
	client := func(n int) bool { return n == 2 } // the first tick is at this listing, after the scan
	stopped, _ := watchFor(t, restartable("make", "test"), &supervisor{pid: 42, group: true}, g, st, 3, client, nil)
	if stopped || kills != 0 {
		t.Errorf("stopped %v after %d kills; want the stop ignored", stopped, kills)
	}
}

// A hit whose hold update failed is written by a later tick, or the hold stays a victim for good.
// Mutation: write the mark only on the listing of the hit → Victim stays true, red.
func TestHitUpdateIsRetried(t *testing.T) {
	g, st := watchedHold(t)
	format := filepath.Join(st.Dir(), store.FormatFile)
	each := func(n int) {
		switch n {
		case 1: // a run writes no hold under a FORMAT that is gone
			if err := os.Rename(format, format+".away"); err != nil {
				t.Fatal(err)
			}
		case 3:
			if err := os.Rename(format+".away", format); err != nil {
				t.Fatal(err)
			}
			if h := body(t, st, g); !h.Victim {
				t.Error("the failed update was written")
			}
		}
	}
	client := func(int) bool { return true }
	watchFor(t, restartable("make", "test"), &supervisor{pid: 42, group: true}, g, st, 4, client, each)
	if h := body(t, st, g); h.Victim || !h.Container {
		t.Errorf("Victim %v Container %v; want false, true on the later tick", h.Victim, h.Container)
	}
}

// A run outside the budget has no grant and still writes a record, so it scans; its unbudgeted
// hold stays as it is. Mutation: move the scan below the g == nil skip → no mark, red.
func TestNoGrantStillDetects(t *testing.T) {
	_, st := watchedHold(t)
	_, p := watchFor(t, restartable("make", "test"), &supervisor{pid: 42}, nil, st, 1, func(int) bool { return true }, nil)
	if !p.container {
		t.Error("peak has no mark; want it for the record")
	}
}

// A pass-through writes no record, so nothing would read the scan. Mutation: drop st != nil from
// the scan's condition → a scan, red.
func TestPassThroughDoesNotScan(t *testing.T) {
	scans := 0
	watchFor(t, restartable("make", "test"), &supervisor{pid: 42}, nil, nil, 1, func(int) bool { scans++; return true }, nil)
	if scans != 0 {
		t.Errorf("%d scans; want none", scans)
	}
}

// A run with no hit is what it was: a victim with no key. Mutation: write Victim false in the first
// update → false, red.
func TestNoHitLeavesVictim(t *testing.T) {
	g, st := watchedHold(t)
	_, p := watchFor(t, restartable("make", "test"), &supervisor{pid: 42, group: true}, g, st, 2, func(int) bool { return false }, nil)
	raw, err := os.ReadFile(filepath.Join(st.Dir(), store.HoldsDir, g.HoldName()))
	if err != nil {
		t.Fatal(err)
	}
	if h := body(t, st, g); !h.Victim || h.Container || p.container || strings.Contains(string(raw), "container") {
		t.Errorf("hold %s, peak %+v; want a victim with no key, no mark", raw, p)
	}
}
