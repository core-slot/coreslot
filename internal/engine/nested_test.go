//go:build unix

package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// A run is inside its parent's reservation only when CORESLOT_HOLD names a hold that is live in
// the state directory the context names (never this process's environment) and whose pid is an
// ancestor. Mutations: skipping the probe reddens "dead"; skipping isAncestor reddens "not an
// ancestor"; reading the process environment's CORESLOT_STATE_DIR reddens "live ancestor".
func TestDecideNested(t *testing.T) {
	dir := newStateDir(t, "")
	e := newEngine(t, dir, newFakeClock())
	live := hold(t, e, need(128)).HoldName()
	deadName := wire.HoldName(7, 4321, 1, "dead") // a hold nobody locks, left by a killed run
	if err := os.WriteFile(filepath.Join(dir, store.HoldsDir, deadName), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.StateDirEnv, t.TempDir()) // not the context's: it must not count
	livePID := func(name string) int { id, _ := wire.ParseName(name); return id.PID }(live)
	ctx := func(hold string) []string {
		return []string{config.StateDirEnv + "=" + dir, EnvHold + "=" + hold}
	}
	for _, tc := range []struct {
		name     string
		env      []string
		ancestor bool
		inside   bool
	}{
		{name: "live ancestor", env: ctx(live), ancestor: true, inside: true},
		{name: "a later duplicate wins", env: append(ctx("x"), EnvHold+"="+live), ancestor: true, inside: true},
		{name: "not an ancestor", env: ctx(live), ancestor: false},
		{name: "dead", env: ctx(deadName), ancestor: true},
		{name: "gone", env: ctx(wire.HoldName(5, livePID, 9, "gone")), ancestor: true},
		{name: "not a hold name", env: ctx("../holds"), ancestor: true},
		{name: "unset", env: []string{config.StateDirEnv + "=" + dir}, ancestor: true},
		{name: "relative state directory", env: []string{config.StateDirEnv + "=state", EnvHold + "=" + live}, ancestor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := 0
			inside, _ := DecideNested(tc.env, func(pid int) bool {
				asked = pid
				return tc.ancestor
			})
			if inside != tc.inside {
				t.Fatalf("DecideNested(%v) inside %v, want %v", tc.env, inside, tc.inside)
			}
			if tc.inside && asked != livePID {
				t.Errorf("isAncestor asked about pid %d, want the hold's %d", asked, livePID)
			}
		})
	}
}
