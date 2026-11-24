//go:build coreslot_testhooks && unix

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/testhooks"
)

// setReadings replaces the named machine readings through the readings hook (a new file each time,
// so that its cache reads it again).
func setReadings(t *testing.T, dir string, nums map[string]int64) {
	t.Helper()
	var b strings.Builder
	for name, n := range nums {
		fmt.Fprintf(&b, "%s %d\n", name, n)
	}
	path := filepath.Join(dir, testhooks.ReadingsFile)
	if err := os.WriteFile(path+".partial", []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".partial", path); err != nil {
		t.Fatal(err)
	}
}

// hostAt is a host whose swap used is swapUsedMiB, with room to spare, or with none for floor.
func hostAt(swapUsedMiB int64, floor bool) map[string]int64 {
	r := map[string]int64{"Decomp": 0, "SwapIns": 0, "HasPSI": 0, "SwapUsedMiB": swapUsedMiB,
		"SwapFreeMiB": 1 << 20, "VMVolumeFreeMiB": 1 << 20}
	if floor {
		r["SwapFreeMiB"], r["VMVolumeFreeMiB"], r["MemAvailableMiB"] = 0, 0, 0
	}
	return r
}

// Under an observe override with the configuration saying enforce, a stop round is written in
// observe mode by both ticks: a running hold's (Grant.Tick) and a restart waiter's at its admission
// (watchWait). Mutation: either tick reading the configuration's mode instead of Config's writes an
// enforce round here.
func TestRoundUnderTheOverrideThroughEachTick(t *testing.T) {
	for _, tick := range []string{"hold", "restart"} {
		t.Run(tick, func(t *testing.T) {
			hooks := t.TempDir()
			t.Setenv("CORESLOT_TESTHOOK_DIR", hooks)
			clk := newFakeClock()
			e := newEngine(t, newStateDir(t, ""), clk)
			setWatchMode(t, e, "observe", 0)
			g := hold(t, e, need(128))
			req := need(128)
			req.Restart = true
			var restart *Ticket
			if tick == "restart" {
				restart = enqueue(t, e, req)
			}
			for i, h := range []map[string]int64{hostAt(0, false), hostAt(1024, true)} {
				setReadings(t, hooks, h)
				if i > 0 {
					clk.advance(3 * time.Second) // the next period's sample
				}
				if restart != nil {
					step(t, restart)
				} else if _, err := g.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			d, _, _, err := e.st.ReadWatch()
			if err != nil || d.LastRoundNS == 0 || !d.Observed {
				t.Errorf("after a floor sample through the %s tick: round at %d, observed %v (%v); want a round in observe mode",
					tick, d.LastRoundNS, d.Observed, err)
			}
		})
	}
}
