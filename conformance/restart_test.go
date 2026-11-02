//go:build coreslot_testhooks && unix

package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/runstats"
	"github.com/core-slot/coreslot/internal/store"
)

// stopB starts A, then B with flags, and stops B's command once from the floor at swapUsedMiB;
// the machine is left at the floor. It returns B's run once B's command is gone, its marker
// removed so that a restart can be told apart.
func stopB(t *testing.T, e Env, flags ...string) *Proc {
	t.Helper()
	e.Learn("B")
	e.Run("A", "--mem", "100M")
	e.WaitStarted("A")
	e.WaitTicked("A")
	b := e.Run("B", append([]string{"--mem", "100M"}, flags...)...)
	stopAgain(t, e, b, 1024)
	return b
}

// stopAgain waits until B's command has started and ticked, puts the machine at the floor with
// swapUsedMiB of swap used and waits until the command is killed.
func stopAgain(t *testing.T, e Env, b *Proc, swapUsedMiB int64) {
	t.Helper()
	kid := e.WaitStarted("B")
	e.WaitTicked("B")
	e.SetReadings(floor(swapUsedMiB))
	e.Eventually(startWait, "B's command is killed", func() bool { return !alive(kid.PID) || b.Exited() })
	if err := os.Remove(filepath.Join(e.MarkDir, "B.started")); err != nil {
		t.Fatal(err)
	}
}

// A stopped command comes back with its first arrival: queued ahead of C, which arrived after
// it and waits for room, and admitted while C still waits. Mutation: re-enqueueing with a new
// arrival queues B behind C.
func TestRestartKeepsPlace(t *testing.T) {
	e := State(t)
	e.WriteConfig(roomConfig(1000) + "[watch]\ncalm_s = 0\n")
	e.Learn("B")
	a := e.Run("A", "--mem", "300M")
	e.WaitStarted("A")
	e.WaitTicked("A")
	b := e.Run("B", "--mem", "300M")
	e.WaitStarted("B")
	c := e.Run("C", "--mem", "500M")
	e.WaitTicked("B")
	e.WaitTicket("C")
	stopAgain(t, e, b, 1024)
	e.WaitTicket("B")
	if tk := e.Status().Tickets; len(tk) != 2 || tk[0].Label != "B" || tk[1].Label != "C" {
		t.Fatalf("queue %+v, want B's ticket ahead of C's", tk)
	}
	e.SetReadings(calm(2048))
	e.WaitStarted("B")
	if _, waiting := e.Ticket("C"); !waiting {
		t.Error("C is no longer waiting once B restarted")
	}
	for _, name := range []string{"A", "B", "C"} {
		e.ReleaseChild(name)
	}
	a.ExitCode(0)
	b.ExitCode(0)
	c.ExitCode(0)
}

// The stopped attempt writes its own run-log line, with its stop, which learning does not count.
// Mutation: logging the stopped attempt with stops 0 puts its line in the window.
func TestStoppedAttemptLogged(t *testing.T) {
	e := State(t)
	e.WriteConfig(watchConfig(""))
	b := stopB(t, e)
	e.SetReadings(calm(2048))
	e.WaitStarted("B")
	e.ReleaseChild("A")
	e.ReleaseChild("B")
	b.ExitCode(0)
	st, err := store.OpenReadOnly(e.Dir)
	if err != nil {
		t.Fatal(err)
	}
	runs, _, err := st.ReadRuns()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range runs {
		if r.Label == "B" {
			lines = append(lines, fmt.Sprintf("%d/%d", r.Exit, r.Stops))
		}
	}
	if got := strings.Join(lines, " "); got != "0/0 137/1 0/0" {
		t.Errorf("B's run-log lines (exit/stops) %q, want \"0/0 137/1 0/0\"", got)
	}
	if w := runstats.Window(runs, "B", runstats.WindowRuns); len(w) != 2 || w[1].Exit != 0 {
		t.Errorf("B's window %+v, want the seeded run and the completed attempt", w)
	}
}

// After the third stop the run gives up with the refusal code, the marker line first, and
// restarts nothing more; after a floor stop its last line asks for memory freed outside coreslot.
// Mutations: giving up after the fourth stop leaves B waiting; a give-up without the ask lacks it.
func TestGiveUpAfterThreeStops(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		code int
	}{{"default", nil, 75}, {"refusal code 70", []string{"--refusal-code", "70"}, 70}} {
		t.Run(c.name, func(t *testing.T) {
			e := State(t).on(t)
			e.WriteConfig(watchConfig("post_kill_s = 0\n"))
			b := stopB(t, e, c.args...)
			for k := int64(2); k <= 3; k++ {
				e.SetReadings(calm(2048 * (k - 1)))
				stopAgain(t, e, b, 2048*(k-1)+1024)
			}
			b.ExitCode(c.code)
			out := b.Stderr()
			marker := strings.Index(out, "coreslot: gave up after 3 memory stops (run ")
			if marker < 0 || strings.Count(out, "restarts it by itself") != 2 ||
				strings.LastIndex(out, "restarts it by itself") > marker {
				t.Errorf("stderr:\n%s\nwant two restart notices, then the marker line", out)
			}
			if n := strings.Count(out[:marker], "the whole machine ran out of memory (swap nearly full); free memory outside coreslot"); n != 2 {
				t.Errorf("stderr:\n%s\nwant both floor stops to ask for memory freed outside coreslot, got %d", out, n)
			}
			block := out[marker:]
			for _, want := range []string{"coreslot: stop 1: swap room under the floor, at ", "coreslot: stop 3: ",
				"coreslot: run `", "` alone, or narrow it; the whole machine ran out of memory (swap nearly full); free memory outside coreslot (close something)\n"} {
				if !strings.Contains(block, want) {
					t.Errorf("the give-up block\n%s\nlacks %q", block, want)
				}
			}
		})
	}
}

// A stopped --no-wait run does not wait for calm: it refuses at its first restart and says that
// its command was stopped for memory and not restarted because of --no-wait, and after a floor stop
// asks for memory freed outside coreslot. Mutation: restarting regardless leaves B waiting.
func TestNoWaitRefusesAtRestart(t *testing.T) {
	e := State(t)
	e.WriteConfig(watchConfig(""))
	b := stopB(t, e, "--no-wait")
	b.ExitCode(75)
	if out := b.Stderr(); !strings.Contains(out, "was stopped for memory (swap room under the floor, at ") ||
		!strings.Contains(out, ") and not restarted because of --no-wait; the whole machine ran out of memory (swap nearly full); free memory outside coreslot (close something)\n") {
		t.Errorf("stderr:\n%s\nwant the stop and the refusal's reason", out)
	}
	if _, restarted := e.Started("B"); restarted {
		t.Error("B's command started again")
	}
}

// --timeout counts waiting: B's restart, which waits for calm past it, refuses with the stop's
// reason and never runs outside the budget, even with --on-timeout run. Mutation: --on-timeout
// run at a restart starts B's command again.
func TestRestartTimeoutRefuses(t *testing.T) {
	e := State(t)
	e.WriteConfig(roomConfig(64 << 10)) // calm_s 30: no restart within the timeout
	b := stopB(t, e, "--timeout", "5s", "--on-timeout", "run")
	b.ExitCode(75)
	if out := b.Stderr(); !strings.Contains(out, "was stopped for memory (swap room under the floor, at ") ||
		!strings.Contains(out, ") and not restarted: the run's --timeout of 5s ran out; the whole machine ran out of memory (swap nearly full); free memory outside coreslot (close something)\n") {
		t.Errorf("stderr:\n%s\nwant the stop and the timeout's refusal", out)
	}
	if _, restarted := e.Started("B"); restarted {
		t.Error("B's command started again")
	}
}

// --timeout is one deadline from the run's start: B's command runs 4.5 s of its 6 s, is stopped, and
// the restart, which waits for calm, refuses when the 1.5 s left run out. Mutation: a fresh timeout
// at each wait refuses 6 s after the stop.
func TestTimeoutCoversTheRun(t *testing.T) {
	e := State(t)
	e.WriteConfig(roomConfig(64 << 10)) // calm_s 30: no restart within the timeout
	e.Learn("B")
	e.Run("A", "--mem", "100M")
	e.WaitStarted("A")
	e.WaitTicked("A")
	b := e.Run("B", "--mem", "100M", "--timeout", "6s")
	e.WaitStarted("B")
	e.WaitTicked("B")
	time.Sleep(4500 * time.Millisecond)
	stopAgain(t, e, b, 1024)
	e.Eventually(4*time.Second, "B refuses with the time left", b.Exited)
	b.ExitCode(75)
}
