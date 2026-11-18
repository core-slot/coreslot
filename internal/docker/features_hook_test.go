//go:build unix && coreslot_testhooks

package docker_test

import (
	"reflect"
	"testing"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/docker"
)

// Under a state directory override the test hook's socket is the engine, and only it. Mutation:
// ignoring the hook answers state-dir.
func TestFeaturesStateDirOverrideWithHook(t *testing.T) {
	t.Setenv("CORESLOT_TESTHOOK_DOCKER_SOCKET", "/tmp/x/hook.sock")
	got, off := docker.Features("darwin", true, map[string]string{config.StateDirEnv: "/tmp/state"}, "", false)
	if !reflect.DeepEqual(got, []string{"/tmp/x/hook.sock"}) || off != docker.On {
		t.Errorf("got %q, %q, want the hook's socket, on", got, off)
	}
}
