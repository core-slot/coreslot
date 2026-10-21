//go:build coreslot_testhooks && unix

package conformance

import (
	"strings"
	"testing"
	"time"
)

// TestTraceLiveMemOlderClientWaits: a Go CLI run is admitted above the static mem size from free
// memory, and the independent Python client, which sizes mem statically, waits for it. Static mem
// is 1 GiB (8 tokens) and the headroom is pinned at 4096 MiB, so the run's 2 GiB (16 tokens) gets
// a live size of 32 and runs unclamped; the Python client's 128 MiB then finds 16 locked above
// its size of 8 and stays queued until the run exits.
//
// Mutation: attemptSizes returning the static sizes clamps the run to 8 tokens, so it prints 1024
// instead of 2048.
func TestTraceLiveMemOlderClientWaits(t *testing.T) {
	py, pyc, err := pyClient(t)
	if err != nil {
		t.Skipf("the independent Python client is not exercised: no Python 3.11+ (%v)", err)
	}
	e := State(t)
	e.WriteConfig("[resources]\nmem = \"1G\"\n")

	run := e.StartCommand("/usr/bin/env", "CORESLOT_TESTHOOK_HEADROOM_MIB=4096", bins.coreslot, "run",
		"--label", "go", "--need", "mem=2G", "--", "sh", "-c", `echo $CORESLOT_GRANTED_MEM; sleep 5`)
	e.WaitHold("go", startWait)

	waiter := e.StartCommand(py, pyc, "run", "--label", "py", "--need", "mem=128M", "--",
		bins.child, "hold", e.MarkDir, "py")
	e.WaitTicket("py")
	for !run.Exited() {
		noHold(t, e, "py", "the Go run holds 16 mem tokens above the Python client's size of 8")
		time.Sleep(100 * time.Millisecond)
	}
	run.ExitCode(0)
	if got := strings.TrimSpace(run.Stdout()); got != "2048" {
		t.Fatalf("the Go run was granted %s MiB of mem, want 2048 (unclamped, above the static 1024); stderr:\n%s",
			got, run.Stderr())
	}

	e.WaitStarted("py")
	e.ReleaseChild("py")
	waiter.ExitCode(0)
}
