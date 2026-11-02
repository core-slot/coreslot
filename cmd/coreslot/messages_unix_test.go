//go:build unix

package main

import (
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/watch"
)

// waitReason names the room a request lacks first, then the level that holds admissions, then a
// restart's wait for calm. Mutation: testing the level first says "memory is yellow" for the first.
func TestWaitReason(t *testing.T) {
	holds := []engine.HolderInfo{{Label: "a", NeedMiB: 1024}}
	mem := engine.MemInfo{Known: true, FreeMiB: 1126}
	yellow := engine.WatchInfo{Level: watch.Yellow, Fresh: true}
	for _, c := range []struct {
		rep  engine.Report
		req  spec.Request
		want string
	}{
		{engine.Report{Holds: holds, Mem: mem, Watch: yellow}, spec.Request{NeedMiB: 2458}, "needs 2.4 GiB, 1.1 GiB free"},
		{engine.Report{Holds: holds, Mem: mem, Watch: yellow}, spec.Request{NeedMiB: 512}, "memory is yellow"},
		{engine.Report{Holds: holds, Mem: mem}, spec.Request{NeedMiB: 512, Restart: true},
			"a stopped command restarts when memory has been calm for a while"},
		{engine.Report{Holds: holds, Mem: mem, Watch: engine.WatchInfo{Level: watch.Red}}, spec.Request{NeedMiB: 512}, ""},
	} {
		if got := waitReason(c.rep, c.req); got != c.want {
			t.Errorf("waitReason(%+v) = %q, want %q", c.req, got, c.want)
		}
	}
}

// The stop detail names the level, the footprint and the stalls against the label's normal, a share
// under 1 % to one digit; the command is cut at 200 characters. Mutation: whole percent everywhere
// says "normal 0%" for 0.3 %.
func TestStopMessageParts(t *testing.T) {
	r := &run{o: runOptions{label: "x"}, share: 0.04, learned: true}
	times := memsample.TreeTimes{UserNS: 8e8, SysNS: 6e8, PageWaitNS: 6e8}
	if got, want := r.stopDetail(false, 2458*1024, times),
		"memory red, at 2.4 GiB, 60% of its time went to memory stalls against its normal 4%"; got != want {
		t.Errorf("stopDetail = %q, want %q", got, want)
	}
	r.share = 0.003
	if got, want := r.stopDetail(false, 2458*1024, times),
		"memory red, at 2.4 GiB, 60% of its time went to memory stalls against its normal 0.3%"; got != want {
		t.Errorf("stopDetail with a small normal = %q, want %q", got, want)
	}
	if got, want := r.stopDetail(true, 0, memsample.TreeTimes{}), "swap room under the floor, at 0 MiB"; got != want {
		t.Errorf("stopDetail with no time = %q, want %q", got, want)
	}
	if got := commandText([]string{"go", "test", "./..."}); got != "`go test ./...`" {
		t.Errorf("commandText = %q", got)
	}
	if got := commandText([]string{strings.Repeat("a", 300)}); got != "`"+strings.Repeat("a", 200)+"...`" {
		t.Errorf("commandText of 300 characters = %q", got)
	}
}

// status shows the level and E, whether the watchdog protects and why not, and each job's
// footprint, stops and whether it is the next victim.
func TestStatusWatch(t *testing.T) {
	const now = int64(1e18)
	holds := []engine.HolderInfo{
		{Name: "h1", Label: "go test@repo", PID: 4711, StartNS: now - 192e9, ArrivalNS: now - 200e9, NeedMiB: 2048, FootprintMiB: 1536, Stops: 1},
		{Name: "h2", Label: "npm run build", PID: 4712, StartNS: now - 5e9, ArrivalNS: now - 5e9, NeedMiB: 512, FootprintMiB: 100},
	}
	rep := func(w engine.WatchInfo, h []engine.HolderInfo) engine.Report {
		w.NextVictim = "h2"
		return engine.Report{Holds: h, Watch: w}
	}
	for _, c := range []struct {
		rep  engine.Report
		want string
	}{
		{rep(engine.WatchInfo{Level: watch.Red, Excess: 41, Fresh: true}, holds), "watch: red, E 41.0 points; protecting"},
		// no victim at red, whether the learning job is the newest or the oldest, or none is
		// learning: red lists nobody, so it does not say "protecting"
		{engine.Report{Holds: holds, Watch: engine.WatchInfo{Level: watch.Red, Excess: 41, Fresh: true, Learning: 1}},
			"watch: red, E 41.0 points; not protecting: a red round can stop no running command: each is the oldest, still learning or never stopped"},
		{engine.Report{Holds: holds, Watch: engine.WatchInfo{Level: watch.Red, Excess: 41, Fresh: true}},
			"watch: red, E 41.0 points; not protecting: a red round can stop no running command: each is the oldest, still learning or never stopped"},
		{rep(engine.WatchInfo{Level: watch.Green, Fresh: true, Observe: true}, holds),
			"watch: green, E 0.0 points; not protecting: observe mode logs what it would stop and stops nothing"},
		{rep(engine.WatchInfo{Level: watch.Unknown, Fresh: true}, holds),
			"watch: unknown, E 0.0 points; not protecting: the last sample is a gap (the machine slept, or a reading is missing)"},
		{rep(engine.WatchInfo{Level: watch.Green}, holds),
			"watch: no fresh sample; not protecting: no run has sampled the machine in the last 6 s"},
		{rep(engine.WatchInfo{Level: watch.Green, Fresh: true}, nil),
			"watch: green, E 0.0 points; not protecting: no coreslot job is running"},
	} {
		if out := buildStatus(c.rep, now, nil, 0).human(); !strings.Contains(out, "\n"+c.want+"\n") {
			t.Errorf("status:\n%s\nwant the line %q", out, c.want)
		}
	}
	out := buildStatus(rep(engine.WatchInfo{Level: watch.Yellow, Fresh: true}, holds), now, nil, 0).human()
	if strings.Contains(out, "\nlearning:") {
		t.Errorf("status:\n%s\nwant no learning line when every job has history", out)
	}
	for n, want := range map[int]string{
		1: "learning: 1 running command has no normal share yet; the watchdog does not count such a command, and only the floor can stop it, until a run of its label has measured one",
		2: "learning: 2 running commands have no normal share yet; the watchdog does not count such a command, and only the floor can stop it, until a run of its label has measured one",
	} {
		out := buildStatus(rep(engine.WatchInfo{Level: watch.Green, Fresh: true, Learning: n}, holds), now, nil, 0).human()
		if !strings.Contains(out, "\n"+want+"\n") {
			t.Errorf("status:\n%s\nwant the line %q", out, want)
		}
	}
	for _, want := range []string{
		"\n  go test@repo  pid 4711  mem 2 GiB  held 3m12s  footprint 1.5 GiB  arrived 3m20s ago  stops 1\n",
		"\n  npm run build  pid 4712  mem 512 MiB  held 5s  footprint 100 MiB  arrived 5s ago  next victim\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status:\n%s\nwant the line %q", out, want)
		}
	}
}

// status --json carries the watchdog's state too: the snapshot's learning count and next victim.
// Mutation: leaving Learning out of the document reports 0.
func TestStatusJSONWatch(t *testing.T) {
	w, _ := statusDoc(t, budgetDir(t, 8192), 0)["watch"].(map[string]any)
	if len(w) != 6 || w["level"] != "" || w["protecting"] != false || w["reason"] != "no coreslot job is running" || w["learning"] != float64(0) {
		t.Errorf("status --json watch = %v, want the six keys, no level, not protecting for want of a job, learning 0", w)
	}
	rep := engine.Report{Holds: []engine.HolderInfo{{Name: "h1"}, {Name: "h2"}},
		Watch: engine.WatchInfo{Level: watch.Green, Fresh: true, NextVictim: "h2", Learning: 2}}
	if got := buildStatus(rep, 0, nil, 0).document().Watch; got.Learning != 2 || got.NextVictim != "h2" || got.Level != "green" {
		t.Errorf("status --json watch = %+v, want learning 2, next victim h2, level green", got)
	}
}

// doctor says that each reading is readable, and that no machine sample has been taken yet.
func TestDoctorReadings(t *testing.T) {
	out := cli(t, baseEnv(budgetDir(t, 8192)), "doctor").stdout
	for _, want := range []string{
		"\nreading of paging counters, swap and clocks: readable\n",
		"\nreading of free memory: readable\n",
		"\nreading of process times: readable\n",
		"\nnewest machine sample: none yet (no job has run)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor:\n%s\nwant the line %q", out, want)
		}
	}
}
