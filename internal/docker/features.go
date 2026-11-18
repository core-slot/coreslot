package docker

import (
	"path/filepath"
	"strings"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/testhooks"
)

// Off is why the Docker features are off; "" is on.
type Off string

const (
	On            Off = ""
	OffPlatform   Off = "platform"    // not macOS
	OffConfig     Off = "config"      // [docker] enabled is false or not a boolean, or no configuration could be read
	OffNested     Off = "nested"      // the run inherits a parent's context: its socket would mark its parent's command
	OffStateDir   Off = "state-dir"   // the state directory is overridden: a test or a sandbox
	OffRemoteHost Off = "remote-host" // DOCKER_HOST is not a unix socket
)

// Features decides once whether the Docker features are on, and where the engine is looked for: the
// socket DOCKER_HOST names, or else the usual places under home, the home's two sockets before the
// system's. With no home located there are none: the system's socket alone would reach an engine no
// location of ours belongs to; a DOCKER_HOST that is not a unix socket gives none either. The
// candidates come back whatever the reason: the telemetry line reads as it did before, and only
// config and nested stop it, while status reads none unless on. Whether a candidate exists is for
// the reading to find (NoSocket). When several reasons apply the first of these wins: config,
// nested, platform, state-dir, remote-host. A configuration that could not be read is enabled ==
// false. Under a state directory override (CORESLOT_STATE_DIR in env) a test's
// CORESLOT_TESTHOOK_DOCKER_SOCKET is the engine, and status without one has none (state-dir).
func Features(goos string, enabled bool, env map[string]string, home string, inherited bool) (sockets []string, off Off) {
	host := env["DOCKER_HOST"]
	override := env[config.StateDirEnv] != ""
	hook := testhooks.DockerSocket()
	path, unixHost := strings.CutPrefix(host, "unix://")
	switch {
	case override && hook != "":
		sockets = []string{hook}
	case unixHost:
		sockets = []string{path}
	case host == "" && home != "":
		sockets = []string{filepath.Join(home, ".docker/run/docker.sock"), filepath.Join(home, ".docker/desktop/docker.sock"), "/var/run/docker.sock"}
	}
	switch {
	case !enabled:
		off = OffConfig
	case inherited:
		off = OffNested
	case goos != "darwin":
		off = OffPlatform
	case override && hook == "":
		off = OffStateDir
	case host != "" && !unixHost:
		off = OffRemoteHost
	}
	return sockets, off
}
