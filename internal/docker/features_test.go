//go:build unix

package docker_test

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
)

// socketAt listens on a unix socket in a short directory and returns its path.
func socketAt(t *testing.T) string {
	t.Helper()
	path := filepath.Join(dockertest.SocketDir(t), "docker.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return path
}

func hostFor(sock string) map[string]string {
	return map[string]string{"DOCKER_HOST": "unix://" + sock}
}

// Anything but macOS is off, whatever else is true. Mutation: dropping the goos check turns the
// engine on for Linux, whose Docker is not a desktop VM.
func TestFeaturesOffOnLinux(t *testing.T) {
	sock := socketAt(t)
	if got, off := docker.Features("linux", true, hostFor(sock), "/home/ana", false); got != nil || off != docker.OffPlatform {
		t.Errorf("got %q, %q, want none, platform", got, off)
	}
}

// [docker] enabled = false is off. Mutation: ignoring enabled.
func TestFeaturesOffByConfig(t *testing.T) {
	sock := socketAt(t)
	if got, off := docker.Features("darwin", false, hostFor(sock), "/Users/ana", false); got != nil || off != docker.OffConfig {
		t.Errorf("got %q, %q, want none, config", got, off)
	}
}

// A DOCKER_HOST that is not a unix socket is a remote engine: off, and the marker is never a candidate.
// Mutation: letting the tcp marker through as a socket path.
func TestFeaturesTCPHostIsRemote(t *testing.T) {
	for _, host := range []string{"tcp://10.0.0.1:2375", "ssh://ana@server"} {
		if got, off := docker.Features("darwin", true, map[string]string{"DOCKER_HOST": host}, "/Users/ana", false); got != nil || off != docker.OffRemoteHost {
			t.Errorf("%s: got %q, %q, want none, remote-host", host, got, off)
		}
	}
}

// Only a candidate that is a socket counts: a regular file or an absent path is no engine, and one
// that is a socket is returned. Mutation: dropping the mode check lists the regular file.
func TestFeaturesNoSocketOnList(t *testing.T) {
	dir := dockertest.SocketDir(t)
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, filepath.Join(dir, "absent.sock")} {
		if got, off := docker.Features("darwin", true, hostFor(path), "/Users/ana", false); got != nil || off != docker.OffNoSocket {
			t.Errorf("%s: got %q, %q, want none, no-socket", path, got, off)
		}
	}
	sock := socketAt(t)
	if got, off := docker.Features("darwin", true, hostFor(sock), "/Users/ana", false); !reflect.DeepEqual(got, []string{sock}) || off != docker.On {
		t.Errorf("got %q, %q, want the socket, on", got, off)
	}
}

// /var/run/docker.sock is a symbolic link to the home's socket, and counts. Mutation: os.Lstat in
// place of os.Stat sees a link, not a socket.
func TestFeaturesSymlinkedSocketCounts(t *testing.T) {
	sock := socketAt(t)
	link := filepath.Join(dockertest.SocketDir(t), "link.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	if got, off := docker.Features("darwin", true, hostFor(link), "/Users/ana", false); !reflect.DeepEqual(got, []string{link}) || off != docker.On {
		t.Errorf("got %q, %q, want the link, on", got, off)
	}
}

// Under a state directory override nothing outside it is read: with no hook the answer is
// state-dir and no candidate, whether DOCKER_HOST names a live socket or not. Mutation: letting the
// override fall through to Sockets answers the live socket for the second env and no-socket for the
// first.
func TestFeaturesStateDirOverrideWithoutHook(t *testing.T) {
	t.Setenv("CORESLOT_TESTHOOK_DOCKER_SOCKET", "")
	sock := socketAt(t)
	for _, env := range []map[string]string{{config.StateDirEnv: "/tmp/state"}, {config.StateDirEnv: "/tmp/state", "DOCKER_HOST": "unix://" + sock}} {
		if got, off := docker.Features("darwin", true, env, "", false); off != docker.OffStateDir || got != nil {
			t.Errorf("env %v: got %q, %q, want none, state-dir", env, got, off)
		}
	}
}

// A run inside a parent's context never connects. Mutation: ignoring inherited.
func TestFeaturesNested(t *testing.T) {
	sock := socketAt(t)
	if got, off := docker.Features("darwin", true, hostFor(sock), "/Users/ana", true); got != nil || off != docker.OffNested {
		t.Errorf("got %q, %q, want none, nested", got, off)
	}
}

// When several reasons apply the first in the order wins: platform, config, nested, state-dir,
// remote-host, no-socket. Mutation: any two checks swapped reddens the pair that names them.
func TestFeaturesReasonOrder(t *testing.T) {
	t.Setenv("CORESLOT_TESTHOOK_DOCKER_SOCKET", "")
	all := map[string]string{"DOCKER_HOST": "tcp://10.0.0.1:2375", config.StateDirEnv: "/tmp/state"}
	for _, c := range []struct {
		goos    string
		enabled bool
		env     map[string]string
		nested  bool
		want    docker.Off
	}{
		{"linux", false, all, true, docker.OffPlatform},
		{"darwin", false, all, true, docker.OffConfig},
		{"darwin", true, all, true, docker.OffNested},
		{"darwin", true, all, false, docker.OffStateDir},
		{"darwin", true, map[string]string{"DOCKER_HOST": "tcp://h"}, false, docker.OffRemoteHost},
		{"darwin", true, map[string]string{"DOCKER_HOST": "unix:///nonexistent/x.sock"}, false, docker.OffNoSocket},
	} {
		if _, off := docker.Features(c.goos, c.enabled, c.env, "", c.nested); off != c.want {
			t.Errorf("%s enabled %v env %v nested %v: off %q, want %q", c.goos, c.enabled, c.env, c.nested, off, c.want)
		}
	}
}
