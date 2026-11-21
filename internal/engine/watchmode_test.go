//go:build unix

package engine

import (
	"context"
	"testing"

	"github.com/core-slot/coreslot/internal/store"
)

// setWatchMode writes this machine's override, or removes it for mode "".
func setWatchMode(t *testing.T, e *Engine, mode string, untilNS int64) {
	t.Helper()
	sec, err := e.st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if mode == "" {
		err = sec.RemoveWatchMode()
	} else {
		err = sec.SetWatchMode(mode, untilNS)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// The mode every reader gets from Config, and so from reading (both ticks, the stop rounds, the
// run's records, status), is the override's while it holds and the configuration's otherwise.
// Mutations: Config ignoring the override keeps the configuration's mode in the override rows;
// ignoring until_ns keeps observe in the expired row.
func TestConfigAppliesTheWatchModeOverride(t *testing.T) {
	now := store.NowNS()
	for _, c := range []struct {
		name, config, mode string
		until              int64
		observe            bool
		source             string
	}{
		{"no override, config enforce", "", "", 0, false, "config"},
		{"no override, config observe", "[watch]\nmode = \"observe\"\n", "", 0, true, "config"},
		{"override observe until removed", "", "observe", 0, true, "override"},
		{"override observe until later", "", "observe", now + 3600e9, true, "override"},
		{"override enforce over config observe", "[watch]\nmode = \"observe\"\n", "enforce", 0, false, "override"},
		{"expired override", "", "observe", now - 1e9, false, "config"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEngine(t, newStateDir(t, c.config), newFakeClock())
			setWatchMode(t, e, c.mode, c.until)
			cfg, err := e.Config()
			if err != nil {
				t.Fatal(err)
			}
			rc, _, err := e.reading()
			if err != nil {
				t.Fatal(err)
			}
			_, source, until, err := e.WatchMode(false)
			if err != nil || cfg.Watch.Observe != c.observe || rc.Watch.Observe != c.observe || source != c.source {
				t.Errorf("Config observe %v, reading observe %v, source %q (%v); want %v, %q",
					cfg.Watch.Observe, rc.Watch.Observe, source, err, c.observe, c.source)
			}
			if c.source == "override" && until != c.until {
				t.Errorf("until %d, want %d", until, c.until)
			}
		})
	}
}

// A snapshot says where the mode comes from. Mutation: leaving ModeSource out of the snapshot reads
// "config" under an override.
func TestSnapshotShowsTheModeSource(t *testing.T) {
	e := newEngine(t, newStateDir(t, ""), newFakeClock())
	setWatchMode(t, e, "observe", 0)
	r, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !r.Watch.Observe || r.Watch.ModeSource != "override" || r.Watch.ModeUntilNS != 0 {
		t.Errorf("watch %+v; want observe from the override, until removed", r.Watch)
	}
}
