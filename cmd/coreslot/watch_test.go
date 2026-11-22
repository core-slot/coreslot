//go:build unix

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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
		{[]string{"watch", "observe"}, "in observe mode on this machine, switched by hand until switched back", "observe mode switched by hand until switched back"},
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

// Anything but observe, enforce or config, a --for under a minute and a --for without a mode to
// end are usage errors that change nothing. Mutation: dropping the minimum accepts --for 10s.
func TestWatchCommandUsage(t *testing.T) {
	dir := stateDir(t, "")
	for _, args := range [][]string{{"watch"}, {"watch", "off"}, {"watch", "observe", "enforce"}, {"watch", "observe", "--for", "10s"},
		{"watch", "config", "--for", "1h"}, {"watch", "--for", "1h"}} {
		if r := cli(t, baseEnv(dir), args...); r.code != exitUsage {
			t.Errorf("%q: exit %d (%q); want %d", args, r.code, r.stderr, exitUsage)
		}
	}
	if st := cli(t, baseEnv(dir), "status").stdout; strings.Contains(st, "switched by hand") {
		t.Errorf("after the usage errors status reads %q; want the configuration's mode", st)
	}
}

// status shows the mode with no hold live, in text and in JSON. Mutations: emitting watch.mode only
// while a hold is live leaves it empty here; leaving the override out of the watch line drops
// "switched by hand".
func TestStatusShowsTheWatchMode(t *testing.T) {
	dir := stateDir(t, "")
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
