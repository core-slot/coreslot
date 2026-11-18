package docker

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/testhooks"
)

// Off is why the Docker features are off; "" is on.
type Off string

// The closed reasons.
const (
	On            Off = ""
	OffPlatform   Off = "platform"    // not macOS
	OffConfig     Off = "config"      // [docker] enabled is false or not a boolean, or no configuration could be read
	OffNested     Off = "nested"      // the run inherits a parent's context
	OffStateDir   Off = "state-dir"   // the state directory is overridden: a test or a sandbox
	OffRemoteHost Off = "remote-host" // DOCKER_HOST is not a unix socket
	OffNoSocket   Off = "no-socket"   // no candidate is a socket
)

// Sockets is where the engine is looked for: the socket DOCKER_HOST names, the marker of a host
// that is not a unix socket, or the usual places under home. With no home located there are
// none: the system's socket alone would reach an engine no location of ours belongs to.
func Sockets(host, home string) []string {
	if path, ok := strings.CutPrefix(host, "unix://"); ok {
		return []string{path}
	}
	if host != "" {
		return []string{TCPHost}
	}
	if home == "" {
		return nil
	}
	return []string{filepath.Join(home, ".docker/run/docker.sock"), filepath.Join(home, ".docker/desktop/docker.sock"), "/var/run/docker.sock"}
}

// Features decides once whether the Docker features are on, and where the engine is. When several
// reasons apply the first of these wins: platform, config, nested, state-dir, remote-host,
// no-socket. A configuration that could not be read is enabled == false. Under a state directory
// override (CORESLOT_STATE_DIR in env) a run looked up no home and has no engine of its own: only a
// test's CORESLOT_TESTHOOK_DOCKER_SOCKET gives it one. A candidate counts when it is a socket, or
// a symbolic link to one: /var/run/docker.sock is one.
func Features(goos string, enabled bool, env map[string]string, home string, inherited bool) (sockets []string, off Off) {
	host := env["DOCKER_HOST"]
	override := env[config.StateDirEnv] != ""
	hook, hooked := testhooks.DockerSocket()
	switch {
	case goos != "darwin":
		return nil, OffPlatform
	case !enabled:
		return nil, OffConfig
	case inherited:
		return nil, OffNested
	case override && !hooked:
		return nil, OffStateDir
	case host != "" && !strings.HasPrefix(host, "unix://"):
		return nil, OffRemoteHost
	}
	candidates := Sockets(host, home)
	if override {
		candidates = []string{hook}
	}
	for _, s := range candidates {
		if fi, err := os.Stat(s); err == nil && fi.Mode()&os.ModeSocket != 0 {
			sockets = append(sockets, s)
		}
	}
	if sockets == nil {
		return nil, OffNoSocket
	}
	return sockets, On
}
