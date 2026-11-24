//go:build unix

package engine

import (
	"context"
	"testing"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// setWatchMode writes this machine's override, or removes it for mode "".
func setWatchMode(t *testing.T, e *Engine, mode string, untilNS int64) {
	t.Helper()
	sec, err := e.st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if err := sec.SetWatchMode(mode, untilNS); err != nil {
		t.Fatal(err)
	}
}

// The mode every reader gets from Config, and so from reading (both ticks, the stop rounds, the
// run's records, status), is the override's while it holds and the configuration's otherwise; Config
// also says where it came from and what the configuration says. Mutations: Config ignoring the
// override keeps the configuration's mode in the override rows; ignoring until_ns keeps observe in
// the expired row; recording the effective mode as the configured one reads observe in the
// "override enforce" row.
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
			m := cfg.Mode
			if cfg.Watch.Observe != c.observe || rc.Watch.Observe != c.observe || m.Source != c.source {
				t.Errorf("Config observe %v, reading observe %v, source %q; want %v, %q",
					cfg.Watch.Observe, rc.Watch.Observe, m.Source, c.observe, c.source)
			}
			if c.source == "override" && m.UntilNS != c.until {
				t.Errorf("until %d, want %d", m.UntilNS, c.until)
			}
			if m.ConfiguredObserve != (c.config != "") {
				t.Errorf("configured observe %v, want %v", m.ConfiguredObserve, c.config != "")
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

// A snapshot flags a stop round written in enforce mode whose listed hold is still live and still a
// victim, which its run acts on whatever the mode is now; an observed round, one whose holds are gone,
// or one whose listed run has since found a container client (no victim, it ignores the stop) is no
// such round. Mutations: ignoring d.Observed flags the observed round; ignoring Victim flags the last.
func TestSnapshotFlagsALiveEnforceRound(t *testing.T) {
	for _, c := range []struct {
		name     string
		observed bool
		listLive bool
		victim   bool
		want     bool
	}{{"enforce round, hold live", false, true, true, true}, {"observed round", true, true, true, false},
		{"hold gone", false, false, true, false}, {"listed hold no victim", false, true, false, false}} {
		t.Run(c.name, func(t *testing.T) {
			e := newEngine(t, newStateDir(t, ""), newFakeClock())
			g := hold(t, e, need(64))
			if err := g.UpdateHold(func(h *wire.Hold) { h.Victim = c.victim }); err != nil {
				t.Fatal(err)
			}
			listed := "01795300000000000000-0000000001-000001-gone"
			if c.listLive {
				listed = g.HoldName()
			}
			sec, err := e.st.Enter(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			d, smp, at, err := e.st.ReadWatch()
			if err == nil {
				d.Stop, d.Observed = []string{listed}, c.observed
				err = sec.SetWatch(d, smp, at)
			}
			sec.Leave()
			if err != nil {
				t.Fatal(err)
			}
			r, err := e.Snapshot()
			if err != nil || r.Watch.EnforceRoundLive != c.want {
				t.Errorf("EnforceRoundLive %v (%v); want %v", r.Watch.EnforceRoundLive, err, c.want)
			}
		})
	}
}
