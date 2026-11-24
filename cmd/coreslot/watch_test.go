//go:build unix

package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
)

// watch sets an override with or without an end and returns to the configuration, and status
// follows each step. Mutation: writing the override's end as 0 makes status read "until switched
// back" after --for.
func TestWatchCommandSwitchesThisMachine(t *testing.T) {
	dir := stateDir(t, "")
	for _, c := range []struct {
		args       []string
		say, state string
	}{
		{[]string{"watch", "observe", "--for", "1h"}, "in observe mode on this machine, switched by hand until ", "observe mode switched by hand until "},
		{[]string{"watch", "observe"}, "in observe mode on this machine, switched by hand until switched back", "observe mode switched by hand until switched back, the configuration says enforce"},
		{[]string{"watch", "--for", "2h", "enforce"}, "in enforce mode on this machine, switched by hand until ", "enforce mode switched by hand until "},
		{[]string{"watch", "config"}, "follows the configuration's mode", "watch: no fresh sample; not protecting: no coreslot job is running\n"},
	} {
		r := cli(t, baseEnv(dir), c.args...)
		st := cli(t, baseEnv(dir), "status").stdout
		if r.code != 0 || !strings.Contains(r.stdout, c.say) || !strings.Contains(st, c.state) {
			t.Errorf("%q: exit %d, stdout %q, stderr %q, status %q; want 0, %q and %q", c.args, r.code, r.stdout, r.stderr, st, c.say, c.state)
		}
		if c.args[1] == "observe" && len(c.args) == 4 && strings.Contains(st, "until switched back") {
			t.Errorf("after --for 1h status reads %q", st)
		}
	}
}

// Anything but observe, enforce or config, a --for that is not positive, a --for whose end is past
// the clock's range and a --for without a mode to end are usage errors that change nothing.
// Mutations: accepting --for 0 writes an override with no end; dropping the range check writes the
// overflowed end as 0, an override with no end, and exits 0.
func TestWatchCommandUsage(t *testing.T) {
	dir := stateDir(t, "")
	for _, args := range [][]string{{"watch"}, {"watch", "off"}, {"watch", "observe", "enforce"}, {"watch", "observe", "--for", "0s"},
		{"watch", "observe", "--for", "2500000h"}, {"watch", "config", "--for", "1h"}, {"watch", "--for", "1h"}} {
		if r := cli(t, baseEnv(dir), args...); r.code != exitUsage {
			t.Errorf("%q: exit %d (%q); want %d", args, r.code, r.stderr, exitUsage)
		}
	}
	if st := cli(t, baseEnv(dir), "status").stdout; strings.Contains(st, "switched by hand") {
		t.Errorf("after the usage errors status reads %q; want the configuration's mode", st)
	}
	none := filepath.Join(t.TempDir(), "none")
	if r := cli(t, baseEnv(none), "watch", "observe"); r.code != exitRefusal || !strings.Contains(r.stderr, "no state directory yet") {
		t.Errorf("watch with no state directory: exit %d, stderr %q; want %d naming it", r.code, r.stderr, exitRefusal)
	}
}

// status shows the mode with no hold live, in text and in JSON, and the configuration's mode beside
// an override. Mutations: emitting watch.mode only while a hold is live leaves it empty here; leaving
// the override out of the watch line drops "switched by hand"; naming the effective mode as the
// configuration's reads "the configuration says observe".
func TestStatusShowsTheWatchMode(t *testing.T) {
	dir := stateDir(t, "")
	if r := cli(t, baseEnv(dir), "watch", "enforce"); r.code != 0 {
		t.Fatalf("watch enforce: %d %q", r.code, r.stderr)
	}
	writeConfig(t, dir, "[watch]\nmode = \"observe\"\n")
	if r := cli(t, baseEnv(dir), "status"); !strings.Contains(r.stdout, "enforce mode switched by hand until switched back, the configuration says observe") {
		t.Errorf("status text lacks the configuration's mode beside the override:\n%s", r.stdout)
	}
	writeConfig(t, dir, "")
	if r := cli(t, baseEnv(dir), "watch", "observe"); r.code != 0 {
		t.Fatalf("watch observe: %d %q", r.code, r.stderr)
	}
	r := cli(t, baseEnv(dir), "status", "--json")
	var doc struct {
		Watch struct {
			Mode        string `json:"mode"`
			ModeSource  string `json:"mode_source"`
			ModeUntilNS *int64 `json:"mode_until_ns"`
		} `json:"watch"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	if doc.Watch.Mode != "observe" || doc.Watch.ModeSource != "override" || doc.Watch.ModeUntilNS == nil || *doc.Watch.ModeUntilNS != 0 {
		t.Errorf("watch %+v; want observe from the override, mode_until_ns 0:\n%s", doc.Watch, r.stdout)
	}
	if r := cli(t, baseEnv(dir), "status"); !strings.Contains(r.stdout, "observe mode switched by hand until switched back") {
		t.Errorf("status text lacks the override:\n%s", r.stdout)
	}
}

// A run's enqueue record carries the effective mode. Mutation: the record reading the configured
// mode (config.Load) records enforce under the override.
func TestEnqueueRecordsTheEffectiveMode(t *testing.T) {
	b := newBed(t, 1024)
	if err := b.e.SetWatchModeContext(context.Background(), "observe", 0); err != nil {
		t.Fatal(err)
	}
	b.admit(t)
	evs := events(t, b.dir)
	if len(evs) == 0 || evs[0]["mode"] != "observe" {
		t.Errorf("enqueue %v; want mode observe", evs)
	}
}

// An end on another day than now carries its date; an end today only its time. Mutation: formatting
// every end as 15:04 drops the date two days ahead.
func TestSwitchedTextDatesAnotherDay(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.Local)
	for _, c := range []struct {
		until time.Time
		want  string
	}{
		{now.Add(time.Hour), "switched by hand until 11:00"},
		{now.Add(48 * time.Hour), "switched by hand until 2026-11-26 10:00"},
	} {
		if got := switchedText(c.until.UnixNano(), now.UnixNano()); got != c.want {
			t.Errorf("switchedText(%v) = %q; want %q", c.until, got, c.want)
		}
	}
}

// Under observe, status says a stop round written in enforce mode still stops what it listed; under
// enforce, or with no such round, it says nothing of it. Mutation: dropping the clause, or its
// Observe condition, fails a row.
func TestStatusNotesALiveEnforceRound(t *testing.T) {
	const note = "a stop round written before the switch still stops the jobs it listed"
	for _, c := range []struct {
		w    engine.WatchInfo
		want bool
	}{
		{engine.WatchInfo{Observe: true, EnforceRoundLive: true}, true},
		{engine.WatchInfo{Observe: false, EnforceRoundLive: true}, false},
		{engine.WatchInfo{Observe: true}, false},
	} {
		v := statusView{rep: engine.Report{Watch: c.w}, nowNS: 1}
		if got := strings.Contains(v.watchLine(), note); got != c.want {
			t.Errorf("%+v: the line notes the round: %v; want %v\n%s", c.w, got, c.want, v.watchLine())
		}
	}
}

// status dates an override's end against its own clock: an end later the same day prints only its
// time. Mutations: passing 0 or the wall clock now instead of the view's nowNS dates it.
func TestStatusDatesTheOverrideByItsClock(t *testing.T) {
	now := time.Date(2025, 3, 10, 10, 0, 0, 0, time.Local)
	v := statusView{rep: engine.Report{Watch: engine.WatchInfo{Observe: true, ModeSource: "override",
		ModeUntilNS: now.Add(time.Hour).UnixNano()}}, nowNS: now.UnixNano()}
	if line := v.watchLine(); !strings.Contains(line, "switched by hand until 11:00,") {
		t.Errorf("watch line %q; want the end's time alone", line)
	}
}
