//go:build coreslot_testhooks && unix

package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Nezhinskiy/coreslot"
	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/store"
)

// Bounds of the traces. I5 lets a stopped head block the queue for 11 s; the traces allow
// twice that, because OS scheduling of the processes involved is not part of the claim.
const (
	stalledHeadBound = 22 * time.Second
	pollBound        = 100 * time.Millisecond // a library Poll while the section is held
	noWaitBound      = 2 * time.Second        // --no-wait refusing while the section is held
	waitExpiryBound  = time.Second            // Wait returning after its deadline
	// observeWait lets every waiter observe a change: a waiter elects once a second, and a
	// head before each admission attempt, at most 2 s apart.
	observeWait = 5 * time.Second
)

const exitRefusal = 75

// ---------------------------------------------------------------- helpers

func cpuConfig(n int) string { return fmt.Sprintf("[resources]\ncpu = %d\n", n) }

func admittedAt(h coreslot.HolderInfo) time.Time { return time.Unix(0, h.StartNS) }

// noHold fails the test when a live hold labelled label exists.
func noHold(t *testing.T, e Env, label, why string) {
	t.Helper()
	if h, ok := e.Hold(label); ok {
		t.Fatalf("%s holds %v (admitted at %v): %s", label, h.Grant, admittedAt(h), why)
	}
}

// sectionFreeOf fails unless the section is free of pid: a process must be stopped outside it.
func sectionFreeOf(t *testing.T, e Env, pid int) {
	t.Helper()
	if s := e.Status().Section; s.Held && s.PID == pid {
		t.Fatalf("pid %d was stopped inside the admission section", pid)
	}
}

// stat returns the device and inode of path.
func stat(t *testing.T, path string) [2]uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return [2]uint64{uint64(st.Dev), uint64(st.Ino)}
}

// checkDescriptors checks the descriptor contract (PROTOCOL.md section 17) of a run's
// command: fd 3 is the hold's file, the tokens follow from fd 4 in ascending index, and the
// environment names them.
func checkDescriptors(t *testing.T, e Env, r ChildReport, holdName string, grant map[string][]int) {
	t.Helper()
	if got := r.FDs[3]; got != stat(t, filepath.Join(e.Dir, store.HoldsDir, holdName)) {
		t.Errorf("fd 3 is %v, not the hold %s", got, holdName)
	}
	fd := 4
	for _, name := range sortedKeys(grant) {
		for _, i := range grant[name] {
			if got, want := r.FDs[fd], stat(t, filepath.Join(e.Dir, store.ResDir, name, strconv.Itoa(i))); got != want {
				t.Errorf("fd %d is %v, not token %s/%d %v", fd, got, name, i, want)
			}
			fd++
		}
	}
	if r.Env["CORESLOT_HELD_FD"] != "3" {
		t.Errorf("CORESLOT_HELD_FD = %q, want 3", r.Env["CORESLOT_HELD_FD"])
	}
	// A single descriptor may be written 4 or 4-4 (PROTOCOL.md section 17).
	want := []string{fmt.Sprintf("4-%d", fd-1)}
	if fd == 5 {
		want = append(want, "4")
	}
	if got := r.Env["CORESLOT_TOKEN_FDS"]; fd > 4 && !slices.Contains(want, got) {
		t.Errorf("CORESLOT_TOKEN_FDS = %q, want %v", got, want)
	}
	for _, n := range sortedFDs(r.FDs) {
		if n >= fd {
			t.Errorf("fd %d is open beyond the grant (which ends at fd %d)", n, fd-1)
		}
	}
}

func sortedFDs(fds map[int][2]uint64) []int {
	out := make([]int, 0, len(fds))
	for fd := range fds {
		out = append(out, fd)
	}
	slices.Sort(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// runLog reads log/runs.jsonl.
func runLog(t *testing.T, e Env) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(e.Dir, store.LogDir, store.RunLog))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("run log line %q: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	return lines
}

func logLine(t *testing.T, e Env, label string) map[string]any {
	t.Helper()
	for _, l := range runLog(t, e) {
		if l["label"] == label {
			return l
		}
	}
	t.Fatalf("no run log line for %s", label)
	return nil
}

// python returns a Python 3.11+ interpreter for the independent client.
func python() (string, error) {
	py, err := exec.LookPath("python3")
	if err == nil {
		err = exec.Command(py, "-c", "import sys; sys.exit(sys.version_info < (3, 11))").Run()
	}
	return py, err
}

// ---------------------------------------------------------------- environment traces

// The default macOS state directory contains a space. Every CLI command, the descriptor
// mapping and the independent Python client work there unchanged, sharing one budget.
func TestStateDirWithSpace(t *testing.T) {
	e := State(t)
	if !strings.Contains(e.Dir, " ") {
		t.Fatalf("state directory %q has no space", e.Dir)
	}
	e.WriteConfig(cpuConfig(2))

	g := e.Run("go", "--need", "cpu=2")
	r := e.WaitStarted("go")
	h := e.WaitHold("go", startWait)
	checkDescriptors(t, e, r, h.Name, e.HoldBody(h.Name).Grant)
	if r.Env["CORESLOT_GRANTED_CPU"] != "2" || r.Env["CORESLOT_STATE_DIR"] != e.Dir {
		t.Errorf("environment %v", r.Env)
	}

	// status, human and --json, while one run holds and one waits.
	w := e.Run("waiting", "--need", "cpu=1")
	tkW := e.WaitTicket("waiting")
	st := e.Start("status")
	st.ExitCode(0)
	for _, want := range []string{"go", "waiting", tkW.ID} {
		if !strings.Contains(st.Stdout(), want) {
			t.Errorf("status does not show %q:\n%s", want, st.Stdout())
		}
	}
	sj := e.Start("status", "--json")
	sj.ExitCode(0)
	var doc struct {
		StateDir string `json:"state_dir"`
		Holds    []struct {
			Label string         `json:"label"`
			Grant map[string]int `json:"grant"`
		} `json:"holds"`
		Tickets []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal([]byte(sj.Stdout()), &doc); err != nil {
		t.Fatalf("status --json: %v\n%s", err, sj.Stdout())
	}
	if doc.StateDir != e.Dir || len(doc.Holds) != 1 || doc.Holds[0].Label != "go" || doc.Holds[0].Grant["cpu"] != 2 ||
		len(doc.Tickets) != 1 || doc.Tickets[0].ID != tkW.ID {
		t.Errorf("status --json: %+v", doc)
	}

	// cancel, by id and by label.
	e.Start("cancel", tkW.ID).ExitCode(0)
	w.ExitCode(exitRefusal)
	w2 := e.Run("waiting2", "--need", "cpu=1")
	e.WaitTicket("waiting2")
	e.Start("cancel", "--label", "waiting2").ExitCode(0)
	w2.ExitCode(exitRefusal)

	t.Run("python", func(t *testing.T) {
		py, err := python()
		if err != nil {
			if os.Getenv("CORESLOT_REQUIRE_PYTHON") == "1" {
				t.Fatalf("CORESLOT_REQUIRE_PYTHON=1 and no Python 3.11+: %v", err)
			}
			t.Skipf("the independent Python client is not exercised: no Python 3.11+ (%v)", err)
		}
		e := e.on(t)
		root, err := moduleRoot()
		if err != nil {
			t.Fatal(err)
		}
		client := filepath.Join(root, "conformance", "pyclient", "coreslot_client.py")
		p := e.StartCommand(py, client, "run", "--need", "cpu=1", "--label", "py", "--",
			bins.child, "hold", e.MarkDir, "py")
		e.WaitTicket("py")

		// The Python client cancels a Go run's ticket in the same directory.
		v := e.Run("victim", "--need", "cpu=1")
		tk := e.WaitTicket("victim")
		e.StartCommand(py, client, "cancel", tk.ID).ExitCode(0)
		v.ExitCode(exitRefusal)

		e.ReleaseChild("go")
		pr := e.WaitStarted("py")
		ph := e.WaitHold("py", startWait)
		checkDescriptors(t, e, pr, ph.Name, e.HoldBody(ph.Name).Grant)
		e.ReleaseChild("py")
		p.ExitCode(0)
	})
	e.ReleaseChild("go")
	g.ExitCode(0)
	if l := logLine(t, e, "go"); l["exit"] != float64(0) || l["fail_open"] != "" {
		t.Errorf("run log line %v", l)
	}
	if rep := e.Status(); len(rep.Holds) != 0 || len(rep.Tickets) != 0 || rep.Degraded != nil {
		t.Errorf("state after the runs: holds %v, tickets %v, degraded %v", rep.Holds, rep.Tickets, rep.Degraded)
	}
	d := e.Start("doctor")
	if code := d.Wait(); code != 0 {
		t.Errorf("doctor exited %d in the directory with a space; stdout:\n%s\nstderr:\n%s", code, d.Stdout(), d.Stderr())
	}
}

// With no configuration file at all the budget is cpu = 100%, there is no degraded mode, and a
// run prints nothing.
func TestFreshMachineRunsQuietly(t *testing.T) {
	e := State(t)
	if _, err := os.Stat(e.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the state directory exists before the first run: %v", err)
	}
	p := e.Run("fresh")
	r := e.WaitStarted("fresh")
	e.ReleaseChild("fresh")
	p.ExitCode(0)
	if got := p.Stderr() + p.Stdout(); got != "" {
		t.Errorf("a run on a fresh machine printed %q", got)
	}
	if r.Env["CORESLOT_GRANTED_CPU"] != "1" || r.Env["CORESLOT_UNBUDGETED"] != "" {
		t.Errorf("environment %v", r.Env)
	}
	cpus, err := config.MachineCPUs()
	if err != nil {
		t.Fatal(err)
	}
	rep := e.Status()
	if len(rep.Resources) != 1 || rep.Resources["cpu"].Size != cpus {
		t.Errorf("resources %v, want only cpu of size %d", rep.Resources, cpus)
	}
	if rep.Degraded != nil || rep.ConfigError != "" || rep.FormatError != "" || rep.Format != 1 {
		t.Errorf("degraded %v, config error %q, format %d %q", rep.Degraded, rep.ConfigError, rep.Format, rep.FormatError)
	}
	for _, name := range []string{store.ConfigFile, store.DegradedFile} {
		if _, err := os.Stat(filepath.Join(e.Dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after a quiet run (%v)", name, err)
		}
	}
	if l := logLine(t, e, "fresh"); l["fail_open"] != "" || l["exit"] != float64(0) {
		t.Errorf("run log line %v", l)
	}
}

// ---------------------------------------------------------------- stalled heads

// startStoppableHead starts a run that pauses before every admission attempt and is released
// automatically, so that the trace can later catch it outside the section and stop it there.
func startStoppableHead(e Env, name string, flags ...string) (*Proc, func()) {
	e.t.Helper()
	p := e.Pausing("admit.before-enter").Run(name, flags...)
	return p, p.AutoRelease("admit.before-enter")
}

// stopOutsideSection catches p before an admission attempt, outside the section, stops it
// there and releases the pause point, which p consumes once continued. It returns the time
// of the stop and p's ticket mtime at that time.
func stopOutsideSection(e Env, p *Proc, autoRelease func()) (time.Time, int64) {
	e.t.Helper()
	autoRelease()
	p.WaitReached("admit.before-enter")
	p.Stop()
	at := time.Now()
	p.Release("admit.before-enter")
	sectionFreeOf(e.t, e, p.PID())
	tk, ok := e.Ticket(p.name)
	if !ok || tk.Dead {
		e.t.Fatalf("the stopped head %s has no live ticket", p.name)
	}
	return at, tk.MtimeNS
}

// waitRefreshed waits until the ticket labelled label carries an mtime other than before.
func waitRefreshed(e Env, label string, before int64) {
	e.t.Helper()
	e.Eventually(startWait, label+" refreshes its ticket", func() bool {
		tk, ok := e.Ticket(label)
		return ok && !tk.Dead && tk.MtimeNS != before
	})
}

// A head stopped with no other change to the queue stops blocking it: its successor is admitted
// within the bound, and once continued the old head takes its place again and is admitted next.
func TestStoppedHeadNoOtherChange(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(2))
	x := e.Run("X", "--need", "cpu=1")
	e.WaitHold("X", startWait)

	// A needs both tokens and waits at the head; B and C need one, which is free, but there is
	// no backfill: they wait behind the live head.
	a, autoA := startStoppableHead(e, "A", "--need", "cpu=2")
	e.WaitTicket("A")
	b := e.Run("B", "--need", "cpu=1")
	e.WaitTicket("B")
	c := e.Run("C", "--need", "cpu=1")
	e.WaitTicket("C")
	noHold(t, e, "B", "B was backfilled past the live head A")

	stoppedAt, mtime := stopOutsideSection(e, a, autoA)
	e.SectionLeft() // C's enqueue section, ticket observed above, has bumped gen and left
	gen := e.Status().Gen
	hb := e.WaitHold("B", stalledHeadBound+startWait)
	if at := admittedAt(hb); at.Before(stoppedAt) {
		t.Fatalf("B was admitted at %v, before A stopped at %v", at, stoppedAt)
	} else if d := at.Sub(stoppedAt); d > stalledHeadBound {
		t.Fatalf("B was admitted %v after the head stopped, bound %v (load: %s)", d, stalledHeadBound, uptime())
	} else {
		t.Logf("B was admitted %v after the head stopped", d)
	}
	if got := e.Status().Gen; got != gen+1 {
		t.Errorf("gen went from %d to %d: something besides B's admission changed the queue", gen, got)
	}
	noHold(t, e, "C", "C was admitted past B")

	a.Cont()
	defer a.AutoRelease("admit.before-enter")()
	waitRefreshed(e, "A", mtime)
	time.Sleep(observeWait)

	// One token frees up: C would fit, A (needing two) does not, and C, which saw A stall, must
	// now see it live again and wait behind it. Then the second token: A is next.
	e.ReleaseChild("B")
	b.ExitCode(0)
	time.Sleep(observeWait)
	noHold(t, e, "C", "C was admitted past the resumed head A")
	e.ReleaseChild("X")
	x.ExitCode(0)
	e.WaitHold("A", startWait)
	noHold(t, e, "C", "C was admitted before the resumed head A")
	e.ReleaseChild("A")
	a.ExitCode(0)
	e.WaitHold("C", startWait)
	e.ReleaseChild("C")
	c.ExitCode(0)
}

// Several consecutive stalled tickets are all passed over, by a waiter that saw them stop and by
// one that first sees them already stopped; continued, they are admitted in their order again.
func TestConsecutiveStalledTickets(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(3))
	x := e.Run("X", "--need", "cpu=1")
	e.WaitHold("X", startWait)

	a, autoA := startStoppableHead(e, "A", "--need", "cpu=3")
	e.WaitTicket("A")
	b := e.Run("B", "--need", "cpu=3")
	e.WaitTicket("B")
	d := e.Run("D", "--need", "cpu=1")
	e.WaitTicket("D")
	noHold(t, e, "D", "D was backfilled past the live head A")

	// B waits behind the live head and never enters the section once its enqueue section (and
	// D's, after it) has ended; A is caught outside it.
	e.SectionLeft()
	b.Stop()
	sectionFreeOf(t, e, b.PID())
	tkB, _ := e.Ticket("B")
	stoppedAt, mtimeA := stopOutsideSection(e, a, autoA)

	// C arrives after both stopped: its first sighting of them is already stale. E arrives too,
	// and finds no token left once C and D are admitted.
	c := e.Run("C", "--need", "cpu=1")
	tkC := e.WaitTicket("C")
	f := e.Run("E", "--need", "cpu=1")
	e.WaitTicket("E")

	hd := e.WaitHold("D", stalledHeadBound+startWait)
	if dd := admittedAt(hd).Sub(stoppedAt); dd > stalledHeadBound {
		t.Fatalf("D was admitted %v after the heads stopped, bound %v (load: %s)", dd, stalledHeadBound, uptime())
	}
	hc := e.WaitHold("C", stalledHeadBound+startWait)
	if dc := admittedAt(hc).Sub(time.Unix(0, tkC.CreatedNS)); dc > stalledHeadBound {
		t.Fatalf("C was admitted %v after it arrived behind stopped tickets, bound %v (load: %s)", dc, stalledHeadBound, uptime())
	}
	t.Logf("D was admitted %v after the heads stopped; C %v after it arrived",
		admittedAt(hd).Sub(stoppedAt), admittedAt(hc).Sub(time.Unix(0, tkC.CreatedNS)))
	if !admittedAt(hd).Before(admittedAt(hc)) {
		t.Errorf("C (admitted %v) went before D (admitted %v), which is ahead of it", admittedAt(hc), admittedAt(hd))
	}

	a.Cont()
	defer a.AutoRelease("admit.before-enter")()
	b.Cont()
	waitRefreshed(e, "A", mtimeA)
	waitRefreshed(e, "B", tkB.MtimeNS)
	time.Sleep(observeWait)

	// One token frees up: E would fit, A and B (needing three) do not, and E, which saw them
	// stall, must now see them live again and wait behind them.
	e.ReleaseChild("D")
	d.ExitCode(0)
	time.Sleep(observeWait)
	noHold(t, e, "E", "E was admitted past the resumed tickets A and B")
	for _, name := range []string{"C", "X"} {
		e.ReleaseChild(name)
	}
	for _, p := range []*Proc{c, x} {
		p.ExitCode(0)
	}
	e.WaitHold("A", startWait)
	noHold(t, e, "B", "B was admitted beside A")
	noHold(t, e, "E", "E was admitted before the resumed head A")
	e.ReleaseChild("A")
	a.ExitCode(0)
	e.WaitHold("B", startWait)
	noHold(t, e, "E", "E was admitted before the resumed B")
	e.ReleaseChild("B")
	b.ExitCode(0)
	e.WaitHold("E", startWait)
	e.ReleaseChild("E")
	f.ExitCode(0)
}

// ---------------------------------------------------------------- order

// A higher-priority ticket published after the head won an election, while the head waits
// before a section that cannot admit it, is admitted first: the head repeats the election
// before its next attempt (PROTOCOL.md section 10.3). One published after the head's election
// but before the section that could admit it is admitted first too: that section repeats the
// election (section 12.1 step 5), where a ticket first seen counts as live. One published while
// the head is inside the section that admits it is admitted after it.
func TestArrivalBeforeAndAfterElection(t *testing.T) {
	t.Run("admissible P published between H's election and its admitting section", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(cpuConfig(1))
		h := e.Pausing("admit.before-enter", "section.entered").Run("H", "--need", "cpu=1")
		autoSec := h.AutoRelease("section.entered")
		e.WaitTicket("H")
		h.WaitReached("admit.before-enter") // H won an election with the token free
		autoSec()

		// P (priority 10), ahead of H, is published and elected too, and is held before its
		// own admitting section.
		p := e.Pausing("admit.before-enter").Run("P", "--priority", "10", "--need", "cpu=1")
		e.WaitTicket("P")
		p.WaitReached("admit.before-enter")
		if rep := e.Status(); len(rep.Tickets) != 2 || rep.Tickets[0].Label != "P" {
			t.Fatalf("queue %v: P is not ahead of H", rep.Tickets)
		}

		// H enters the section it was elected for, with the token free: it must not take it.
		h.Release("admit.before-enter")
		h.WaitReached("section.entered")
		defer h.AutoRelease("admit.before-enter")()
		h.Release("section.entered")
		defer h.AutoRelease("section.entered")()
		e.Eventually(startWait, "H leaves its section", func() bool { return !e.Status().Section.Held })
		noHold(t, e, "H", "H was admitted past P, published before its admitting section")

		p.Release("admit.before-enter")
		defer p.AutoRelease("admit.before-enter")()
		e.WaitHold("P", startWait)
		noHold(t, e, "H", "H was admitted beside P")
		e.ReleaseChild("P")
		p.ExitCode(0)
		e.WaitHold("H", startWait)
		e.ReleaseChild("H")
		h.ExitCode(0)
	})
	e := State(t)
	e.WriteConfig("[resources]\ncpu = 1\npg = 1\n")
	x := e.Run("X", "--need", "cpu=1")
	e.WaitHold("X", startWait)
	y := e.Run("Y", "--need", "pg=1")
	e.WaitHold("Y", startWait)

	h := e.Pausing("admit.before-enter", "section.entered", "publish.after-create").Run("H", "--need", "cpu=1")
	autoPub := h.AutoRelease("publish.after-create")
	autoSec := h.AutoRelease("section.entered")
	e.WaitTicket("H")
	h.WaitReached("admit.before-enter") // H won an election: it is the head
	autoSec()

	// Before H's next section, P (priority 10) is published, ahead of H in the order.
	p := e.Run("P", "--priority", "10", "--need", "cpu=1", "--need", "pg=1")
	e.WaitTicket("P")
	if rep := e.Status(); len(rep.Tickets) != 2 || rep.Tickets[0].Label != "P" {
		t.Fatalf("queue %v: P is not ahead of H", rep.Tickets)
	}

	// H goes on into the section it was elected for; X holds the cpu token, so it is not
	// admitted and leaves.
	h.Release("admit.before-enter")
	h.WaitReached("section.entered")
	defer h.AutoRelease("admit.before-enter")()
	h.Release("section.entered")
	defer h.AutoRelease("section.entered")()
	e.Eventually(startWait, "H leaves its section", func() bool { return !e.Status().Section.Held })

	// The cpu token frees up. P cannot be admitted (Y holds pg) and H, behind the live P, must
	// not be: it repeats the election before each attempt.
	e.ReleaseChild("X")
	x.ExitCode(0)
	time.Sleep(observeWait)
	noHold(t, e, "H", "H was admitted past P: it did not repeat the election before its next attempt")
	e.ReleaseChild("Y")
	y.ExitCode(0)
	e.WaitHold("P", startWait)
	noHold(t, e, "H", "H was admitted beside P")

	// H is the head again. P2 (priority 10) arrives while H is paused inside the section that
	// admits it: H's admission stands, and P2 is admitted after H.
	autoPub()
	e.ReleaseChild("P")
	p.ExitCode(0)
	h.CatchPublication("publish.after-create", store.HoldsDir)
	p2 := e.Run("P2", "--priority", "10", "--need", "cpu=1")
	time.Sleep(time.Second) // P2 starts and tries to enter the section H holds
	if rep := e.Status(); !rep.Section.Held || rep.Section.PID != h.PID() {
		t.Fatalf("section %+v: H (pid %d) is not inside its admitting section", rep.Section, h.PID())
	}
	if _, ok := e.Ticket("P2"); ok {
		t.Fatal("P2 published a ticket while H held the section")
	}
	h.Release("publish.after-create")
	defer h.AutoRelease("publish.after-create")()
	hh := e.WaitHold("H", startWait)
	tk2 := e.WaitTicket("P2")
	if !admittedAt(hh).Before(time.Unix(0, tk2.CreatedNS)) {
		t.Errorf("P2 arrived at %v, before H's admission at %v", time.Unix(0, tk2.CreatedNS), admittedAt(hh))
	}
	noHold(t, e, "P2", "P2 was admitted beside H")
	e.ReleaseChild("H")
	h.ExitCode(0)
	e.WaitHold("P2", startWait)
	e.ReleaseChild("P2")
	p2.ExitCode(0)
}

// ---------------------------------------------------------------- publication

var publishPoints = []string{"publish.after-create", "publish.after-lock", "publish.after-write", "publish.before-rename"}

// A writer paused at each point of a publication, for each published file type, loses nothing
// when it goes on and a reconciliation follows; a writer killed at each point leaves only dead
// files, which a reconciliation removes, restoring the state from before it started.
func TestPublishPausedAtEveryPoint(t *testing.T) {
	for _, kind := range []string{"ticket", "hold", "cancel"} {
		for _, point := range publishPoints {
			for _, killed := range []bool{false, true} {
				outcome := "released"
				if killed {
					outcome = "killed"
				}
				t.Run(kind+"/"+strings.TrimPrefix(point, "publish.")+"/"+outcome, func(t *testing.T) {
					e := State(t)
					e.WriteConfig(cpuConfig(2))
					// FORMAT and gen exist, and a reconciliation just ran.
					e.Start("run", "--no-wait", "--", "true").ExitCode(0)
					switch kind {
					case "ticket":
						publishTicket(t, e, point, killed)
					case "hold":
						publishHold(t, e, point, killed)
					case "cancel":
						publishCancel(t, e, point, killed)
					}
				})
			}
		}
	}
}

// restored checks the state after a forced reconciliation: no temporary file, nothing dead,
// nothing ignored, and exactly the live entries listed.
func restored(t *testing.T, e Env, tickets, holds, cancels []string, locked int) {
	t.Helper()
	if tmps := e.AllTmps(); len(tmps) != 0 {
		t.Errorf("temporary files after the reconciliation: %v", tmps)
	}
	rep := e.Status()
	var gotT, gotH []string
	for _, tk := range rep.Tickets {
		if tk.Dead {
			t.Errorf("dead ticket %s after the reconciliation", tk.Name)
		}
		gotT = append(gotT, tk.Label)
	}
	for _, h := range rep.Holds {
		gotH = append(gotH, h.Label)
	}
	if !slices.Equal(gotT, tickets) || !slices.Equal(gotH, holds) || len(rep.DeadHolds) != 0 || !slices.Equal(rep.Cancels, cancels) {
		t.Errorf("tickets %v (want %v), holds %v (want %v), dead holds %v, cancels %v",
			gotT, tickets, gotH, holds, rep.DeadHolds, rep.Cancels)
	}
	for dir, n := range rep.Ignored {
		if n != 0 {
			t.Errorf("%d ignored entries in %s", n, dir)
		}
	}
	if got := rep.Resources["cpu"].Locked; got != locked {
		t.Errorf("%d cpu tokens locked, want %d", got, locked)
	}
}

func publishTicket(t *testing.T, e Env, point string, killed bool) {
	x := e.Run("X", "--need", "cpu=2")
	e.WaitHold("X", startWait)
	w := e.Pausing(point).Run("W", "--need", "cpu=1")
	w.CatchPublication(point, store.QueueDir)
	if _, ok := e.Ticket("W"); ok {
		t.Fatal("W's ticket is visible before its publication completed")
	}
	if killed {
		w.Kill()
		if tmps := e.Tmps(store.QueueDir, w.PID()); len(tmps) != 1 {
			t.Fatalf("the killed writer left %v in queue/", tmps)
		}
		if n := e.Reconcile(); n < 1 {
			t.Errorf("the reconciliation removed %d entries", n)
		}
		restored(t, e, nil, []string{"X"}, nil, 2)
		z := e.Run("Z", "--need", "cpu=1")
		e.WaitTicket("Z")
		e.ReleaseChild("X")
		x.ExitCode(0)
		e.WaitHold("Z", startWait)
		e.ReleaseChild("Z")
		z.ExitCode(0)
		return
	}
	w.Release(point)
	defer w.AutoRelease(point)()
	e.WaitTicket("W")
	if n := e.Reconcile(); n != 0 {
		t.Errorf("the reconciliation removed %d entries with nothing dead", n)
	}
	if tk, ok := e.Ticket("W"); !ok || tk.Dead {
		t.Fatal("the reconciliation lost W's live ticket")
	}
	if _, ok := e.Hold("X"); !ok {
		t.Fatal("the reconciliation lost X's live hold")
	}
	restored(t, e, []string{"W"}, []string{"X"}, nil, 2)
	e.ReleaseChild("X")
	x.ExitCode(0)
	e.WaitHold("W", startWait)
	e.ReleaseChild("W")
	w.ExitCode(0)
}

func publishHold(t *testing.T, e Env, point string, killed bool) {
	w := e.Pausing(point).Run("W", "--need", "cpu=1")
	w.CatchPublication(point, store.HoldsDir) // inside the admitting section, tokens taken
	if rep := e.Status(); len(rep.Holds) != 0 || len(rep.DeadHolds) != 0 {
		t.Fatalf("a hold is visible before W's publication completed: holds %v, dead %v", rep.Holds, rep.DeadHolds)
	}
	if killed {
		w.Kill()
		if tmps := e.Tmps(store.HoldsDir, w.PID()); len(tmps) != 1 {
			t.Fatalf("the killed writer left %v in holds/", tmps)
		}
		if tk, ok := e.Ticket("W"); !ok || !tk.Dead {
			t.Fatalf("the killed writer's ticket: %+v (present %v), want dead", tk, ok)
		}
		if n := e.Reconcile(); n < 2 {
			t.Errorf("the reconciliation removed %d entries, want the ticket and the temporary", n)
		}
		restored(t, e, nil, nil, nil, 0)
		e.Start("run", "--no-wait", "--need", "cpu=2", "--", "true").ExitCode(0)
		return
	}
	w.Release(point)
	defer w.AutoRelease(point)()
	r := e.WaitStarted("W")
	e.WaitHold("W", startWait)
	if n := e.Reconcile(); n != 0 {
		t.Errorf("the reconciliation removed %d entries with nothing dead", n)
	}
	h, ok := e.Hold("W")
	if !ok || h.Grant["cpu"] != 1 || r.Env["CORESLOT_GRANTED_CPU"] != "1" {
		t.Fatalf("after the reconciliation W's hold is %+v (present %v), its command saw %v", h, ok, r.Env)
	}
	restored(t, e, nil, []string{"W"}, nil, 1)
	e.ReleaseChild("W")
	w.ExitCode(0)
}

func publishCancel(t *testing.T, e Env, point string, killed bool) {
	x := e.Run("X", "--need", "cpu=2")
	e.WaitHold("X", startWait)
	v := e.Run("V", "--need", "cpu=1")
	e.WaitTicket("V")
	w := e.Run("W", "--need", "cpu=1")
	tk := e.WaitTicket("W")
	// W waits behind the head V and never enters the section once its enqueue section has
	// ended. Stopped, it cannot act on its cancel request, so the request must survive the
	// reconciliation on its own.
	e.SectionLeft()
	w.Stop()
	sectionFreeOf(t, e, w.PID())

	k := e.Pausing(point).StartChild("cancel", e.Dir, tk.ID)
	k.CatchPublication(point, store.CancelDir)
	if rep := e.Status(); len(rep.Cancels) != 0 {
		t.Fatalf("a cancel request is visible before its publication completed: %v", rep.Cancels)
	}
	if killed {
		k.Kill()
		if tmps := e.Tmps(store.CancelDir, k.PID()); len(tmps) != 1 {
			t.Fatalf("the killed canceller left %v in cancel/", tmps)
		}
		if n := e.Reconcile(); n < 1 {
			t.Errorf("the reconciliation removed %d entries", n)
		}
		restored(t, e, []string{"V", "W"}, []string{"X"}, nil, 2)
		w.Cont()
		e.Pausing().StartChild("cancel", e.Dir, tk.ID).ExitCode(0)
		w.ExitCode(exitRefusal)
	} else {
		k.Release(point)
		defer k.AutoRelease(point)()
		k.ExitCode(0)
		if n := e.Reconcile(); n != 0 {
			t.Errorf("the reconciliation removed %d entries with nothing dead", n)
		}
		rep := e.Status()
		if !slices.Contains(rep.Cancels, tk.ID) {
			t.Fatalf("the reconciliation lost the cancel request of W's live ticket: cancels %v", rep.Cancels)
		}
		restored(t, e, []string{"V", "W"}, []string{"X"}, []string{tk.ID}, 2)
		w.Cont()
		w.ExitCode(exitRefusal)
	}
	if got, ok := e.Ticket("W"); ok && !got.Dead {
		t.Errorf("the cancelled W left a live ticket")
	}
	e.ReleaseChild("X")
	x.ExitCode(0)
	e.WaitHold("V", startWait)
	e.ReleaseChild("V")
	v.ExitCode(0)
}

// ---------------------------------------------------------------- admission edges

// A minimum above the resource's size is clamped: the run waits for the whole resource, runs
// alone with it, says so once, and records the requested minimum.
func TestOversizeIsClamped(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(10))
	o := e.Run("O", "--need", "cpu=16")
	r := e.WaitStarted("O")
	h := e.WaitHold("O", startWait)
	if r.Env["CORESLOT_GRANTED_CPU"] != "10" || h.Grant["cpu"] != 10 || h.Clamped["cpu"] != 16 {
		t.Errorf("command environment %v; hold grant %v, clamped %v", r.Env, h.Grant, h.Clamped)
	}
	checkDescriptors(t, e, r, h.Name, e.HoldBody(h.Name).Grant)
	if got := e.Status().Resources["cpu"].Locked; got != 10 {
		t.Errorf("%d cpu tokens locked, want 10", got)
	}
	y := e.Run("Y", "--need", "cpu=1")
	e.WaitTicket("Y")
	noHold(t, e, "Y", "Y ran beside the clamped run")
	e.ReleaseChild("O")
	o.ExitCode(0)
	if want := "coreslot: cpu: requested 16, budget is 10; running alone with 10\n"; o.Stderr() != want {
		t.Errorf("stderr %q, want %q", o.Stderr(), want)
	}
	if l := logLine(t, e, "O"); fmt.Sprint(l["clamped"]) != "[cpu]" {
		t.Errorf("run log line %v", l)
	}
	e.WaitHold("Y", startWait)
	e.ReleaseChild("Y")
	y.ExitCode(0)
}

// A resource the configuration does not declare is not budgeted: the run is budgeted on its
// declared resources, lists the other in its hold, prints one notice, and records no degraded
// mode.
func TestUndeclaredResourceIsUnbudgeted(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(4))
	u := e.Run("U", "--need", "cpu=2", "--need", "pg-local=1")
	r := e.WaitStarted("U")
	h := e.WaitHold("U", startWait)
	if r.Env["CORESLOT_GRANTED_CPU"] != "2" || r.Env["CORESLOT_GRANTED_PG_LOCAL"] != "1" || r.Env["CORESLOT_UNBUDGETED"] != "" {
		t.Errorf("command environment %v", r.Env)
	}
	checkDescriptors(t, e, r, h.Name, e.HoldBody(h.Name).Grant)
	if h.Unbudgeted || !slices.Equal(h.UnbudgetedResources, []string{"pg-local"}) || len(h.Grant) != 1 {
		t.Errorf("hold %+v", h)
	}
	if _, err := os.Stat(filepath.Join(e.Dir, store.ResDir, "pg-local")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("res/pg-local exists (%v)", err)
	}
	e.ReleaseChild("U")
	u.ExitCode(0)
	lines := strings.Split(strings.TrimSuffix(u.Stderr(), "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "pg-local") || !strings.Contains(lines[0], "not declared") {
		t.Errorf("stderr %q, want one notice naming pg-local", u.Stderr())
	}
	if rep := e.Status(); rep.Degraded != nil {
		t.Errorf("degraded %+v", rep.Degraded)
	}
	if l := logLine(t, e, "U"); fmt.Sprint(l["unbudgeted_resources"]) != "[pg-local]" || l["fail_open"] != "" {
		t.Errorf("run log line %v", l)
	}
}

// A queue directory that already holds 65 536 entries cannot take a ticket: a state error, so
// the run fails open (or refuses under --strict).
func TestFullDirectoryFailsOpen(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(4))
	e.Start("run", "--no-wait", "--", "true").ExitCode(0)
	q := filepath.Join(e.Dir, store.QueueDir)
	for i := range store.MaxEntries {
		f, err := os.OpenFile(filepath.Join(q, fmt.Sprintf("junk-%05d", i)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	f := e.Run("F", "--need", "cpu=2")
	r := e.WaitStarted("F")
	h := e.WaitHold("F", startWait)
	if r.Env["CORESLOT_GRANTED_CPU"] != "2" || r.Env["CORESLOT_UNBUDGETED"] == "" ||
		r.Env["CORESLOT_UNBUDGETED"] == "format" || r.Env["CORESLOT_TOKEN_FDS"] != "" || r.Env["CORESLOT_HELD_FD"] != "3" {
		t.Errorf("command environment %v", r.Env)
	}
	if !h.Unbudgeted || len(h.Grant) != 0 {
		t.Errorf("hold %+v, want an unbudgeted hold with an empty grant", h)
	}
	rep := e.Status()
	if rep.Degraded == nil || !strings.Contains(rep.Degraded.Reason, "full") {
		t.Errorf("degraded %+v, want the full queue recorded", rep.Degraded)
	}
	if rep.Ignored[store.QueueDir] != store.MaxEntries || len(rep.Tickets) != 0 {
		t.Errorf("queue: %d ignored, tickets %v", rep.Ignored[store.QueueDir], rep.Tickets)
	}
	e.ReleaseChild("F")
	f.ExitCode(0)
	if !strings.Contains(f.Stderr(), "full") || !strings.Contains(f.Stderr(), "fail-open") {
		t.Errorf("stderr %q, want the fail-open diagnostic", f.Stderr())
	}

	s := e.Run("S", "--strict")
	s.ExitCode(exitRefusal)
	if _, ok := e.Started("S"); ok {
		t.Error("the strict run's command ran")
	}
}

// ---------------------------------------------------------------- stopped in the section

// A helper stopped inside the admission section blocks every admission (the one exception to
// I5), but no caller hangs on it: Poll returns at once, --no-wait refuses within 2 s, and a
// Wait whose deadline passes returns within 1 s, leaving a dead ticket.
func TestStoppedInSectionDoesNotHangCallers(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(1))
	c := e.Client()
	poller, err := c.Enqueue(coreslot.Request{Label: "poller"})
	if err != nil {
		t.Fatal(err)
	}
	waiter, err := c.Enqueue(coreslot.Request{Label: "waiter"})
	if err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(e.MarkDir, "section")
	s := e.StartChild("section", e.Dir, marker)
	e.Eventually(startWait, "the helper enters the section", func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})
	s.Stop()
	if rep := e.Status(); !rep.Section.Held || rep.Section.PID != s.PID() {
		t.Fatalf("section %+v, want held by the stopped helper (pid %d)", rep.Section, s.PID())
	}

	// The poller is the head: its first Poll tries the section once, later ones on the head's
	// backoff.
	for i := range 6 {
		start := time.Now()
		_, ok, err := poller.Poll()
		d := time.Since(start)
		if d >= pollBound {
			t.Errorf("Poll %d took %v, bound %v (load: %s)", i, d, pollBound, uptime())
		} else {
			t.Logf("Poll %d took %v", i, d)
		}
		if ok || err != nil {
			t.Fatalf("Poll %d: admitted %v, err %v", i, ok, err)
		}
		time.Sleep(300 * time.Millisecond)
	}

	start := time.Now()
	n := e.Run("nowait", "--no-wait")
	code := n.Wait()
	if d := time.Since(start); d >= noWaitBound {
		t.Errorf("--no-wait took %v, bound %v (load: %s)", d, noWaitBound, uptime())
	} else {
		t.Logf("--no-wait refused after %v", d)
	}
	if code != exitRefusal {
		t.Errorf("--no-wait exited %d, want %d; stderr:\n%s", code, exitRefusal, n.Stderr())
	}
	if _, ok := e.Started("nowait"); ok {
		t.Error("the --no-wait run's command ran")
	}
	if want := fmt.Sprintf("held by pid %d", s.PID()); !strings.Contains(n.Stderr(), want) {
		t.Errorf("--no-wait stderr %q does not name the section's holder (%q)", n.Stderr(), want)
	}

	// The poller withdraws, so the waiter is the head and tries the section on every attempt.
	if err := poller.Cancel(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	_, err = waiter.Wait(ctx)
	if late := time.Since(deadline); late >= waitExpiryBound {
		t.Errorf("Wait returned %v after its deadline, bound %v (load: %s)", late, waitExpiryBound, uptime())
	} else {
		t.Logf("Wait returned %v after its deadline", late)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait: %v, want the deadline", err)
	}
	// Removing a withdrawn ticket needs the section, so the tickets stay, dead.
	for _, label := range []string{"poller", "waiter"} {
		if tk, ok := e.Ticket(label); !ok || !tk.Dead {
			t.Errorf("the withdrawn %s: ticket present %v, dead %v; want a dead ticket", label, ok, tk.Dead)
		}
	}

	s.Cont()
	if err := os.WriteFile(marker+".release", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.ExitCode(0)
}
