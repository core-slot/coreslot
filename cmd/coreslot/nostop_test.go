//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// setNoStop marks g's hold do-not-stop, or with on false removes the mark, as `coreslot no-stop` does.
func setNoStop(t *testing.T, st *store.Store, g *engine.Grant, on bool) {
	t.Helper()
	id, _ := wire.ParseName(g.HoldName())
	sec, err := st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if on {
		_, err = sec.Mark(store.NoStopDir, id)
	} else {
		_, err = sec.Unmark(store.NoStopDir, id)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// storeRound writes a stop round into watch.json, as a tick that wrote it would.
func storeRound(t *testing.T, st *store.Store, d watch.Detector) {
	t.Helper()
	sec, err := st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if err := sec.SetWatch(d, watch.Sample{}, store.NowNS()); err != nil {
		t.Fatal(err)
	}
}

// countKills counts the kills of the command's group for the rest of the test.
func countKills(t *testing.T) *int {
	t.Helper()
	kills := 0
	saved := killGroup
	t.Cleanup(func() { killGroup = saved })
	killGroup = func(int) error { kills++; return nil }
	return &kills
}

func withStderr(t *testing.T, r *run) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	r.io.Stderr = f
	return f
}

// A hold marked do-not-stop before its run starts is no victim from its first write. Mutation:
// writing Victim from the flags at the first write makes it one until the first tick.
func TestNoStopFromTheFirstWrite(t *testing.T) {
	g, st := watchedHold(t)
	setNoStop(t, st, g, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	restartable("make", "test").watchChild(ctx, &supervisor{pid: 42, group: true}, g, st)
	if h := body(t, st, g); h.Victim {
		t.Error("a marked hold is a victim at its first write")
	}
}

// A run marked after a round listed it does not stop its command, in enforce and observe mode alike,
// says so, and records the round as acted on: after --off it does not stop on that stale round.
// Mutation: not recording the refused round kills the command at the tick after --off.
func TestNoStopRefusesARound(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(map[bool]string{false: "enforce", true: "observe"}[observed], func(t *testing.T) {
			g, st := watchedHold(t)
			storeRound(t, st, watch.Detector{Level: watch.Red, Stop: []string{g.HoldName()}, LastRoundNS: 1, Observed: observed})
			kills := countKills(t)
			r := restartable("make", "test")
			stderr := withStderr(t, r)
			each := func(n int) {
				switch n {
				case 1:
					setNoStop(t, st, g, true)
				case 3:
					setNoStop(t, st, g, false)
				}
			}
			stopped, _ := watchFor(t, r, &supervisor{pid: 42, group: true}, g, st, 6, func(int) bool { return false }, each)
			if stopped || *kills != 0 {
				t.Errorf("stopped %v after %d kills; want none", stopped, *kills)
			}
			said, _ := os.ReadFile(stderr.Name())
			if n := strings.Count(string(said), "is marked do-not-stop: not stopped (memory red"); n != 1 || strings.Contains(string(said), "would stop") {
				t.Errorf("stderr %q; want the refusal once and no would-stop", said)
			}
		})
	}
}

// After --off a run that refused round R stays out of rounds while the last round it read is R, and
// is a victim again once a tick has read a new round. Mutation: restoring at once makes it a victim
// while R still lists it, and R stays live.
func TestNoStopOffAfterRefusal(t *testing.T) {
	g, st := watchedHold(t)
	storeRound(t, st, watch.Detector{Level: watch.Red, Stop: []string{g.HoldName()}, LastRoundNS: 1})
	countKills(t)
	r := restartable("make", "test")
	withStderr(t, r)
	each := func(n int) {
		switch n {
		case 1:
			setNoStop(t, st, g, true)
		case 3:
			setNoStop(t, st, g, false)
		case 5:
			if body(t, st, g).Victim {
				t.Error("a victim again while the refused round is the last one read")
			}
			storeRound(t, st, watch.Detector{Level: watch.Red, LastRoundNS: 2})
		case 7:
			if body(t, st, g).Victim {
				t.Error("a victim again before a tick has read the new round")
			}
		}
	}
	watchFor(t, r, &supervisor{pid: 42, group: true}, g, st, 8, func(int) bool { return false }, each)
	if !body(t, st, g).Victim {
		t.Error("not a victim after a tick read a new round")
	}
}

// A tick that could not read watch.json (a busy section) reads no round, and restores nothing.
// Mutation: taking its zero detector for a reading of a round other than R restores the victim.
func TestNoStopZeroDetectorRestoresNothing(t *testing.T) {
	g, st := watchedHold(t)
	storeRound(t, st, watch.Detector{Level: watch.Red, Stop: []string{g.HoldName()}, LastRoundNS: 1})
	countKills(t)
	r := restartable("make", "test")
	withStderr(t, r)
	var busy *store.Section
	each := func(n int) {
		switch n {
		case 1:
			setNoStop(t, st, g, true)
		case 3:
			setNoStop(t, st, g, false)
		case 4: // the tick of this listing finds the section busy
			var err error
			if busy, err = st.Enter(context.Background()); err != nil {
				t.Fatal(err)
			}
		case 5:
			busy.Leave()
		}
	}
	watchFor(t, r, &supervisor{pid: 42, group: true}, g, st, 6, func(int) bool { return false }, each)
	if body(t, st, g).Victim {
		t.Error("a victim again after a tick that read no round")
	}
}

// A run that never refused a round is a victim again at its first write after --off: no round can
// list it while it wrote Victim false. Mutation: requiring a later round of every run keeps it out.
func TestNoStopOffWithoutRefusal(t *testing.T) {
	g, st := watchedHold(t)
	each := func(n int) {
		switch n {
		case 1:
			setNoStop(t, st, g, true)
		case 3:
			setNoStop(t, st, g, false)
			if body(t, st, g).Victim {
				t.Error("a victim while marked")
			}
		}
	}
	watchFor(t, restartable("make", "test"), &supervisor{pid: 42, group: true}, g, st, 4, func(int) bool { return false }, each)
	if !body(t, st, g).Victim {
		t.Error("not a victim at the first write after --off")
	}
}

// Container work stays out of rounds after --off: the value restored is the one without the mark. A
// restartable command whose tree holds a container client is such work. Mutation: restoring Victim
// from the flags alone makes it a victim.
func TestNoStopOffKeepsContainerWorkOut(t *testing.T) {
	g, st := watchedHold(t)
	each := func(n int) {
		switch n {
		case 1:
			setNoStop(t, st, g, true)
		case 3:
			setNoStop(t, st, g, false)
		}
	}
	watchFor(t, restartable("make", "test"), &supervisor{pid: 42, group: true}, g, st, 4, func(int) bool { return true }, each)
	if h := body(t, st, g); h.Victim || !h.Container {
		t.Errorf("Victim %v Container %v; want false, true", h.Victim, h.Container)
	}
}
