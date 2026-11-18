//go:build coreslot_testhooks && darwin

package main

import (
	"testing"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/wire"
)

// A stale exported CORESLOT_HOLD names no parent of this run, so it costs nothing: the run still
// reads the socket and says why the hold did not count. Mutation: any non-empty hold counting as
// inherited leaves both rows with no connection.
func TestStaleHoldStillConnects(t *testing.T) {
	docker := sleepingDocker(t)
	for _, c := range []struct{ name, hold, miss string }{
		{"names no ancestor", wire.HoldName(1, 999999, 1, "stale"), "not_ancestor"},
		{"malformed", "parent", "malformed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			conns, _ := fakeEngine(t)
			dir := recordedDir(t, 1024)
			r := cli(t, append(baseEnv(dir), ticksEnv+"=1", engine.EnvHold+"="+c.hold, hookSocket()), "run", "--restart=yes", "--label", "stale", "--mem", "128M", "--", docker)
			if r.code != 0 {
				t.Fatalf("exit %d: %q", r.code, r.stderr)
			}
			if conns.Load() == 0 || len(linesOf(t, dir, "containers")) == 0 {
				t.Errorf("%d connections, %d lines from a run no parent holds", conns.Load(), len(linesOf(t, dir, "containers")))
			}
			if m := event(t, events(t, dir), "enqueue"); m["nested_miss"] != c.miss {
				t.Errorf("nested_miss %v, want %s", m["nested_miss"], c.miss)
			}
		})
	}
}
