//go:build unix

package docker_test

import (
	"reflect"
	"testing"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/docker"
)

// DOCKER_HOST with a unix socket is the only candidate; with none, the home's two sockets come
// before the system's; with no home located there is none: the system's socket alone would reach an
// engine no location of ours belongs to. No candidate is looked at: whether one exists is for the
// reading. Mutations: a unix host still searching the defaults; the home's sockets after the
// system's; restoring the home == "" branch (the system's socket) reddens the last row.
func TestFeaturesCandidates(t *testing.T) {
	t.Setenv("CORESLOT_TESTHOOK_DOCKER_SOCKET", "")
	sys := "/var/run/docker.sock"
	run, desktop := "/Users/ana/.docker/run/docker.sock", "/Users/ana/.docker/desktop/docker.sock"
	for _, c := range []struct {
		name, host, home string
		want             []string
	}{
		{"unix host", "unix:///tmp/x/docker.sock", "/Users/ana", []string{"/tmp/x/docker.sock"}},
		{"defaults", "", "/Users/ana", []string{run, desktop, sys}},
		{"no home located", "", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, off := docker.Features("darwin", true, map[string]string{"DOCKER_HOST": c.host}, c.home, false)
			if !reflect.DeepEqual(got, c.want) || off != docker.On {
				t.Errorf("got %q, %q, want %q, on", got, off, c.want)
			}
		})
	}
}

// Under a state directory override nothing outside it is read: with no hook the answer is
// state-dir and no candidate, whatever DOCKER_HOST names. Mutation: letting the override fall
// through to the candidates answers the DOCKER_HOST socket for the second env.
func TestFeaturesStateDirOverrideWithoutHook(t *testing.T) {
	t.Setenv("CORESLOT_TESTHOOK_DOCKER_SOCKET", "")
	for _, env := range []map[string]string{{config.StateDirEnv: "/tmp/state"}, {config.StateDirEnv: "/tmp/state", "DOCKER_HOST": "unix:///tmp/x/docker.sock"}} {
		if got, off := docker.Features("darwin", true, env, "", false); off != docker.OffStateDir || got != nil {
			t.Errorf("env %v: got %q, %q, want none, state-dir", env, got, off)
		}
	}
}

// Each reason is off with no socket, and when several apply the first in the order wins: platform,
// config, nested, state-dir, remote-host. Mutations: any two checks swapped reddens the pair that
// names them; dropping one check reddens its own row.
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
		{"darwin", true, map[string]string{"DOCKER_HOST": "ssh://ana@server"}, false, docker.OffRemoteHost},
	} {
		if got, off := docker.Features(c.goos, c.enabled, c.env, "", c.nested); off != c.want || got != nil {
			t.Errorf("%s enabled %v env %v nested %v: %q, %q, want no socket and %q", c.goos, c.enabled, c.env, c.nested, got, off, c.want)
		}
	}
}
