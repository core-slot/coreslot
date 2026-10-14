package engine

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/core-slot/coreslot/internal/spec"
)

// The environment of a run's command (PROTOCOL.md section 17). They live in a file without a
// build constraint, as ChildEnv does, so that the Windows build of the CLI names its variables
// the way every other client does.
const (
	EnvTokenFDs   = "CORESLOT_TOKEN_FDS"
	EnvHeldFD     = "CORESLOT_HELD_FD"
	EnvUnbudgeted = "CORESLOT_UNBUDGETED"
	EnvGranted    = "CORESLOT_GRANTED_" // followed by a resource name in environment form
)

// ChildEnv is the CORESLOT_* environment a client gives the command it starts (PROTOCOL.md
// section 17), the one builder of it in this module: CORESLOT_GRANTED_<RES> for each resource of
// granted, in ascending resource name; CORESLOT_HELD_FD=3 when held; CORESLOT_TOKEN_FDS when
// tokens is above 0, naming that many descriptors from fd 4 (`4` for one, `4-<3+tokens>` for
// more); and
// CORESLOT_UNBUDGETED=unbudgeted when unbudgeted is not empty. The order and the spelling are
// what commands and tests see, so TestChildEnv pins them byte for byte.
func ChildEnv(granted map[string]int, held bool, tokens int, unbudgeted string) []string {
	var env []string
	for _, name := range slices.Sorted(maps.Keys(granted)) {
		env = append(env, EnvGranted+spec.EnvName(name)+"="+strconv.Itoa(granted[name]))
	}
	if held {
		env = append(env, EnvHeldFD+"=3")
	}
	switch {
	case tokens == 1:
		env = append(env, EnvTokenFDs+"=4")
	case tokens > 1:
		env = append(env, EnvTokenFDs+"=4-"+strconv.Itoa(3+tokens))
	}
	if unbudgeted != "" {
		env = append(env, EnvUnbudgeted+"="+unbudgeted)
	}
	return env
}

// IsContext reports whether an environment variable is grant context (PROTOCOL.md section 17):
// CORESLOT_GRANTED_*, CORESLOT_HELD_FD, CORESLOT_TOKEN_FDS or CORESLOT_UNBUDGETED.
// CORESLOT_STATE_DIR is not.
func IsContext(key string) bool {
	return strings.HasPrefix(key, EnvGranted) || key == EnvHeldFD || key == EnvTokenFDs || key == EnvUnbudgeted
}
