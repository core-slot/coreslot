package engine

import (
	"strconv"
	"strings"
)

// The environment of a run's command (PROTOCOL.md section 17). They live in a file without a
// build constraint, as ChildEnv does, so that the Windows build of the CLI names its variables
// the way every other client does.
const (
	EnvGrantedMem = "CORESLOT_GRANTED_MEM"
	EnvHold       = "CORESLOT_HOLD"
	EnvUnbudgeted = "CORESLOT_UNBUDGETED"
	EnvTelemetry  = "CORESLOT_TELEMETRY" // telemetry is on for the run this command runs under
	envGranted    = "CORESLOT_GRANTED_"  // the prefix every grant variable has
)

// ChildEnv is the CORESLOT_* environment a client gives the command it starts (PROTOCOL.md
// section 17), the one builder of it in this module: CORESLOT_GRANTED_MEM=grantMiB;
// CORESLOT_HOLD=hold when the command inherits the hold named hold, at descriptor 3; and
// CORESLOT_UNBUDGETED=unbudgeted when unbudgeted is not empty. The order and the spelling are
// what commands and tests see, so TestChildEnv pins them byte for byte.
func ChildEnv(grantMiB int64, hold, unbudgeted string) []string {
	env := []string{EnvGrantedMem + "=" + strconv.FormatInt(grantMiB, 10)}
	if hold != "" {
		env = append(env, EnvHold+"="+hold)
	}
	if unbudgeted != "" {
		env = append(env, EnvUnbudgeted+"="+unbudgeted)
	}
	return env
}

// IsContext reports whether an environment variable is grant context (PROTOCOL.md section 17):
// CORESLOT_GRANTED_*, CORESLOT_HOLD, CORESLOT_UNBUDGETED or CORESLOT_TELEMETRY.
// CORESLOT_STATE_DIR is not.
func IsContext(key string) bool {
	return strings.HasPrefix(key, envGranted) || key == EnvHold || key == EnvUnbudgeted || key == EnvTelemetry
}
