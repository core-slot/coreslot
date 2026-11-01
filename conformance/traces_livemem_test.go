//go:build coreslot_testhooks && unix

package conformance

import (
	"strings"
	"testing"
	"time"
)

// TestTraceLiveMemStaticSizeWaits: a CLI run is admitted above the static mem size from free
// memory, and a run that sizes mem statically (live sizing was switched off by then) waits for
// it. Static mem is 1 GiB and the headroom is pinned at 4096 MiB, so the first run's 2 GiB gets
// a live capacity of 4 GiB and runs unclamped; the second run's 128 MiB then finds 2 GiB reserved
// above its size of 1 GiB and stays queued until the first exits.
//
// Mutation: capacityFor returning the static size clamps the run to 1 GiB, so it prints 1024
// instead of 2048.
func TestTraceLiveMemStaticSizeWaits(t *testing.T) {
	e := State(t)
	e.WriteConfig("[resources]\nmem = \"1G\"\n[memory]\nlive = true\n")

	run := e.StartCommand("/usr/bin/env", "CORESLOT_TESTHOOK_HEADROOM_MIB=4096", bins.coreslot, "run",
		"--label", "go", "--mem", "2G", "--", "sh", "-c", `echo $CORESLOT_GRANTED_MEM; sleep 5`)
	e.WaitHold("go", startWait)

	writeConfigAtomic(e, "[resources]\nmem = \"1G\"\n")
	waiter := e.Run("static", "--mem", "128M")
	e.WaitTicket("static")
	for !run.Exited() {
		noHold(t, e, "static", "the first run reserves 2 GiB above the static size of 1 GiB")
		time.Sleep(100 * time.Millisecond)
	}
	run.ExitCode(0)
	if got := strings.TrimSpace(run.Stdout()); got != "2048" {
		t.Fatalf("the Go run was granted %s MiB of mem, want 2048 (unclamped, above the static 1024); stderr:\n%s",
			got, run.Stderr())
	}

	e.WaitStarted("static")
	e.ReleaseChild("static")
	waiter.ExitCode(0)
}
