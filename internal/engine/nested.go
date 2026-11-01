//go:build unix

package engine

import (
	"path/filepath"
	"strings"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// ParseFD parses one descriptor number of the descriptor contract (PROTOCOL.md section 17): one
// to nine decimal digits and nothing else, no sign and no space (spec.ParseDigits). It serves
// CORESLOT_HELD_FD.
func ParseFD(s string) (int, bool) { return spec.ParseDigits(s) }

// DecideNested decides, from the environment env a run inherited (a later duplicate wins, as for
// exec), whether it runs inside its parent's reservation (PROTOCOL.md section 18): inside when
// CORESLOT_HOLD names a hold whose pid isAncestor reports as an ancestor of this process and
// whose lock some process still holds in the state directory env names (its CORESLOT_STATE_DIR,
// or the default). hold is the CORESLOT_HOLD it read. A hold that is malformed, dead, gone, not
// an ancestor's or in a directory that cannot be resolved makes the run an ordinary request. It
// writes nothing.
func DecideNested(env []string, isAncestor func(pid int) bool) (inside bool, hold string) {
	vars := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	hold = vars[EnvHold]
	id, ok := wire.ParseName(hold)
	if !ok || !isAncestor(id.PID) {
		return false, hold
	}
	dir, _, err := config.StateDirFrom(func(k string) string { return vars[k] })
	if err != nil {
		return false, hold
	}
	locked, err := store.ProbeLocked(filepath.Join(dir, store.HoldsDir, hold))
	return err == nil && locked, hold
}
