//go:build unix

package docker_test

import (
	"net"
	"testing"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
)

// A name and a short id resolve to the container's id, in one call each; a reference the engine
// does not know is its code. Mutation: return the reference instead of the answer's Id, red.
func TestInspectResolvesNameAndShortID(t *testing.T) {
	f := dockertest.HTTP(t, dockertest.WithContainer(dockertest.Container{ID: dockertest.LongID, Name: "db", LimitBytes: mib}))
	for _, ref := range []string{"db", dockertest.LongID[:12]} {
		if id, code := docker.Inspect(f.Sock, ref, dockertest.Soon()); id != dockertest.LongID || code != "" {
			t.Errorf("Inspect(%q) = %q, %q; want the id", ref, id, code)
		}
	}
	if id, code := docker.Inspect(f.Sock, "nosuch", dockertest.Soon()); id != "" || code == "" {
		t.Errorf("Inspect of an unknown name = %q, %q; want a code", id, code)
	}
	if n := len(f.Requests()); n != 3 {
		t.Errorf("%d requests %q, want 3", n, f.Requests())
	}
}

// An answer whose Id is not 64 hexadecimal characters is a failure, never an id the run would put
// into a request. Mutation: drop the IsID test, and the short and hostile ids come back, red.
func TestInspectRefusesAnIdTheEngineDoesNotMake(t *testing.T) {
	for _, body := range []string{`{"Id":"abc"}`, `{"Id":"../` + dockertest.LongID[:61] + `"}`, `{}`, `not json`} {
		f := dockertest.Serve(t, func(c net.Conn, _ string) { dockertest.Reply(c, 200, body) })
		if id, code := docker.Inspect(f.Sock, "db", dockertest.Soon()); id != "" || code != docker.Parse {
			t.Errorf("answer %s: Inspect = %q, %q; want parse", body, id, code)
		}
	}
}
