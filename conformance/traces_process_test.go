//go:build coreslot_testhooks && unix

package conformance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot"
	"github.com/core-slot/coreslot/internal/store"
)

// ---------------------------------------------------------------- helpers

// startWith starts argv like StartCommand, with files as its descriptors from 3 on and
// extraEnv appended to e's environment.
func (e Env) startWith(files []*os.File, extraEnv []string, argv ...string) *Proc {
	e.t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(e.environ(), extraEnv...)
	cmd.ExtraFiles = files
	return e.startCmd(cmd, nil)
}

// ptyProc is a process whose controlling terminal is a PTY; the test reads all its output.
type ptyProc struct {
	*Proc
	master *os.File
	mu     sync.Mutex
	out    bytes.Buffer
}

// startPTY starts argv as the leader of a new session on a new PTY, so that its process group
// is the terminal's foreground group, with extraEnv appended to e's environment.
func (e Env) startPTY(extraEnv []string, argv ...string) *ptyProc {
	e.t.Helper()
	master, tty, err := pty.Open()
	if err != nil {
		e.t.Fatal(err)
	}
	started := false
	defer func() {
		tty.Close() // the session holds its own descriptors once started
		if !started {
			master.Close()
		}
	}()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(e.environ(), extraEnv...)
	p := &ptyProc{master: master}
	p.Proc = e.startCmd(cmd, tty)
	started = true
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			p.mu.Lock()
			p.out.Write(buf[:n])
			p.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	// Cleanups run last-in first-out: the master closes before startCmd's cleanup kills the
	// session, which then gets SIGHUP and SIGKILL alike.
	e.t.Cleanup(func() { master.Close() })
	return p
}

func (p *ptyProc) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

func (p *ptyProc) write(s string) {
	p.t.Helper()
	if _, err := io.WriteString(p.master, s); err != nil {
		p.t.Fatal(err)
	}
}

func (p *ptyProc) waitOutput(want string) {
	p.t.Helper()
	p.env.Eventually(startWait, fmt.Sprintf("terminal output %q", want), func() bool { return strings.Contains(p.output(), want) })
}

// shell starts an interactive shell with job control on a PTY; $BIN is the CLI, $CHILD the
// helper child and $M the marker directory.
func (e Env) shell() *ptyProc {
	e.t.Helper()
	sh := e.startPTY([]string{"PS1=csprompt$ ", "ENV=", "HOME=" + e.t.TempDir(), "BIN=" + bins.coreslot,
		"CHILD=" + bins.child, "M=" + e.MarkDir}, "/bin/sh", "-i")
	sh.waitOutput("csprompt$ ")
	return sh
}

// procState is pid's scheduler state ("T" when stopped), "" once it is gone or a zombie.
func procState(pid int) string {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return ""
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 || i+2 >= len(s) {
			return ""
		}
		if st := strings.ToUpper(s[i+2 : i+3]); st != "Z" && st != "X" {
			return st
		}
		return ""
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if st := strings.TrimSpace(string(out)); err == nil && st != "" && st[0] != 'Z' {
		return st[:1]
	}
	return ""
}

// killGroupOf kills pid's process group, after continuing it, when the test ends if pid is still
// in it: a job a shell started, or a command in its own group, never outlives a failing test.
func killGroupOf(t *testing.T, pid int) {
	t.Helper()
	pg, err := unix.Getpgid(pid)
	if err != nil || pid <= 1 || pg <= 1 || pg == unix.Getpgrp() {
		return
	}
	t.Cleanup(func() {
		// Only while pid is still a member of pg: once it is gone the group id may be reused.
		if cur, err := unix.Getpgid(pid); err != nil || cur != pg {
			return
		}
		_ = unix.Kill(-pg, unix.SIGCONT)
		_ = unix.Kill(-pg, unix.SIGKILL)
	})
}

// received is the list of signals the helper child named name logged.
func received(e Env, name string) []string {
	data, _ := os.ReadFile(filepath.Join(e.MarkDir, name+".signals"))
	return strings.Fields(string(data))
}

func ticks(e Env, name string) int64 {
	fi, err := os.Stat(filepath.Join(e.MarkDir, name+".ticks"))
	if err != nil {
		return 0
	}
	return fi.Size()
}

// settled checks that nothing a run published is left: no hold, live or dead, no ticket, no
// locked cpu token.
func settled(t *testing.T, e Env) {
	t.Helper()
	e.SectionLeft()
	rep := e.Status()
	if len(rep.Holds) != 0 || len(rep.DeadHolds) != 0 || len(rep.Tickets) != 0 || rep.Resources["cpu"].Locked != 0 {
		t.Errorf("left behind: holds %v, dead holds %v, tickets %v, %d cpu tokens locked",
			rep.Holds, rep.DeadHolds, rep.Tickets, rep.Resources["cpu"].Locked)
	}
}

// ---------------------------------------------------------------- nesting

// A run nested under a parent that failed open passes through unbudgeted: it runs with its min
// at once, says so, passes CORESLOT_UNBUDGETED on, and enters no section: no ticket, hold, gen
// bump or degraded record of its own, only its run-log line marked nested.
//
// Mutation: a nested run ignores an inherited CORESLOT_UNBUDGETED (PROTOCOL.md section 18 rule
// 1 dropped): it reads the invalid configuration itself and fails open, recording degraded mode
// a second time and publishing a second unbudgeted hold.
func TestNestedUnderFailOpenParent(t *testing.T) {
	e := State(t)
	e.WriteConfig("[resources]\ncpu = 0\n") // invalid: a state error, so the parent fails open
	p := e.Start("run", "--label", "parent", "--need", "cpu=2", "--", "sh", "-c",
		`"$0" run --label inner --need cpu=3 -- "$1" hold "$2" inner`, bins.coreslot, bins.child, e.MarkDir)
	r := e.WaitStarted("inner")
	e.SectionLeft()
	rep := e.Status()
	if len(rep.Holds) != 1 || rep.Holds[0].Label != "parent" || !rep.Holds[0].FailOpen || len(rep.Tickets) != 0 {
		t.Errorf("holds %+v, tickets %v: want only the parent's unbudgeted hold", rep.Holds, rep.Tickets)
	}
	if rep.Degraded == nil || rep.Degraded.Count != 1 {
		t.Errorf("degraded %+v, want one fail-open recorded", rep.Degraded)
	}
	reason := r.Env["CORESLOT_UNBUDGETED"]
	if r.Env["CORESLOT_GRANTED_CPU"] != "3" || reason == "" || reason == "format" || r.Env["CORESLOT_TOKEN_FDS"] != "" {
		t.Errorf("the nested command's environment %v: want cpu 3 and the parent's reason", r.Env)
	}
	if rep.Degraded != nil && rep.Degraded.Reason != reason {
		t.Errorf("the nested command's reason %q is not the parent's %q", reason, rep.Degraded.Reason)
	}
	gen := rep.Gen
	e.ReleaseChild("inner")
	p.ExitCode(0)
	if !strings.Contains(p.Stderr(), "the parent run is unbudgeted") {
		t.Errorf("no pass-through notice in %q", p.Stderr())
	}
	rep = e.Status()
	if rep.Degraded == nil || rep.Degraded.Count != 1 {
		t.Errorf("degraded %+v after the runs, want one fail-open recorded", rep.Degraded)
	}
	if len(rep.Holds) != 0 || len(rep.DeadHolds) != 0 {
		t.Errorf("holds %v, dead %v after the runs", rep.Holds, rep.DeadHolds)
	}
	if rep.Gen != gen+1 { // the parent's removal of its own hold
		t.Errorf("gen went from %d to %d: the nested run entered a section", gen, rep.Gen)
	}
	inner := logLine(t, e, "inner")
	if inner["nested"] != true || fmt.Sprint(inner["grant"]) != "map[cpu:3]" ||
		fmt.Sprint(inner["unbudgeted_resources"]) != "[cpu]" || inner["fail_open"] != "" {
		t.Errorf("the nested run-log line %v", inner)
	}
	if l := logLine(t, e, "parent"); l["fail_open"] == "" {
		t.Errorf("the parent's run-log line %v", l)
	}
}

// A run nested under a launcher that closed the grant's descriptors refuses at once, with a
// diagnostic naming the cause, instead of queueing behind a parent that holds the whole
// budget; so does one whose inherited token descriptors are open but no longer locked (the
// parent is gone).
//
// Mutation: a context that does not validate falls back to an ordinary request (decide queues
// instead of refusing): a ticket appears behind the parent.
func TestDescriptorClosingLauncherRefuses(t *testing.T) {
	t.Run("launcher closed a descriptor", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(cpuConfig(4))
		p := e.Start("run", "--label", "parent", "--need", "cpu=4", "--", "sh", "-c", `
			: > "$2/start"
			sh -c 'exec 4>&-; exec "$0" run --label inner -- "$1" hold "$2" inner' "$0" "$1" "$2"
			echo $? > "$2/code.partial"; mv "$2/code.partial" "$2/code"
			exec "$1" hold "$2" parent`, bins.coreslot, bins.child, e.MarkDir)
		// The parent's own ticket stays in queue/ until its admitting section, which publishes
		// the hold before it unlinks the ticket, has left; refused fails on any ticket it sees.
		e.WaitHold("parent", startWait)
		e.SectionLeft()
		refused(t, e, filepath.Join(e.MarkDir, "start"))
		if !strings.Contains(p.Stderr(), "descriptor 4 of CORESLOT_TOKEN_FDS") || !strings.Contains(p.Stderr(), "closed") ||
			!strings.Contains(p.Stderr(), "--independent") {
			t.Errorf("stderr %q: want a diagnostic naming the closed descriptor and --independent", p.Stderr())
		}
		e.WaitStarted("parent")
		e.ReleaseChild("parent")
		p.ExitCode(0)
	})

	t.Run("parent gone", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(cpuConfig(4))
		e.Start("run", "--need", "cpu=4", "--", "true").ExitCode(0) // the token files exist
		p := e.StartCommand("sh", "-c", `
			: > "$2/start"
			exec 4<"$3/res/cpu/0" 5<"$3/res/cpu/1"
			CORESLOT_TOKEN_FDS=4-5 CORESLOT_GRANTED_CPU=2 "$0" run --label inner -- "$1" hold "$2" inner
			echo $? > "$2/code.partial"; mv "$2/code.partial" "$2/code"`, bins.coreslot, bins.child, e.MarkDir, e.Dir)
		refused(t, e, filepath.Join(e.MarkDir, "start"))
		p.ExitCode(0)
		if !strings.Contains(p.Stderr(), "does not validate") {
			t.Errorf("stderr %q: want the refusal's diagnostic", p.Stderr())
		}
	})
}

// refused waits for the nested run's exit code in $M/code, failing at once if a ticket is
// published meanwhile, and checks that the run refused without running its command.
func refused(t *testing.T, e Env, start string) {
	t.Helper()
	code := filepath.Join(e.MarkDir, "code")
	e.Eventually(startWait, "the nested run ends", func() bool {
		if tks := e.Status().Tickets; len(tks) != 0 {
			t.Fatalf("the nested run queued instead of refusing: tickets %v", tks)
		}
		if _, ok := e.Started("inner"); ok {
			t.Fatal("the nested run's command runs: it was admitted instead of refusing")
		}
		_, err := os.Stat(code)
		return err == nil
	})
	data, err := os.ReadFile(code)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != strconv.Itoa(exitRefusal) {
		t.Errorf("the nested run exited %s, want %d", got, exitRefusal)
	}
	if _, ok := e.Started("inner"); ok {
		t.Error("the nested run's command ran")
	}
	st, err1 := os.Stat(start)
	en, err2 := os.Stat(code)
	if err1 == nil && err2 == nil {
		t.Logf("the nested run refused %v after the launcher started it", en.ModTime().Sub(st.ModTime()))
	}
}

// ---------------------------------------------------------------- grant truth

// A 1-token request on a free 8-token resource takes one token and leaves 7 acquirable (I2):
// a 7-token request is then admitted at once, and nothing more is.
//
// Mutation: the extra pass ignores the request's max (state.fill's room is free - take): the
// 1-token request takes all 8.
func TestOneOfEightLeavesSeven(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(8))
	one := e.Run("one", "--need", "cpu=1")
	r := e.WaitStarted("one")
	h := e.WaitHold("one", startWait)
	if h.Granted["cpu"] != 1 || r.Env["CORESLOT_GRANTED_CPU"] != "1" || !slices.Equal(indices(e, "one"), []int{0}) {
		t.Fatalf("hold %+v (indices %v), environment %v: want one token, index 0", h, indices(e, "one"), r.Env)
	}
	checkDescriptors(t, e, r, h.Name, e.HoldBody(h.Name).Grant)
	if free, err := e.Client().Available("cpu"); err != nil || free != 7 {
		t.Errorf("Available(cpu) = %d, %v; want 7", free, err)
	}
	seven := e.Run("seven", "--no-wait", "--need", "cpu=7")
	e.WaitStarted("seven") // no snapshot probes the tokens while seven takes them
	if got := indices(e, "seven"); !slices.Equal(got, span(1, 7)) {
		t.Errorf("seven holds %v, want 1-7", got)
	}
	e.Start("run", "--no-wait", "--", "true").ExitCode(exitRefusal)
	for _, p := range []*Proc{one, seven} {
		e.ReleaseChild(p.name)
		p.ExitCode(0)
	}
	settled(t, e)
}

// A command that closes every descriptor of its grant while the run lives keeps the grant:
// the run's own copies hold the tokens and the hold until the command exits.
//
// Mutation: the run closes its copies as soon as the command has started (runChild calls
// release before superviseChild): the tokens are free while the command runs.
func TestChildClosingCopiesKeepsTokens(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(2))
	p := e.Start("run", "--label", "closer", "--need", "cpu=2", "--", "sh", "-c",
		`exec 3<&- 4<&- 5<&-; exec "$0" hold "$1" closer`, bins.child, e.MarkDir)
	r := e.WaitStarted("closer")
	if len(r.FDs) != 0 {
		t.Fatalf("the command still has descriptors open: %v", r.FDs)
	}
	time.Sleep(300 * time.Millisecond)
	e.SectionLeft()
	rep := e.Status()
	if rep.Resources["cpu"].Locked != 2 || len(rep.Holds) != 1 || len(rep.DeadHolds) != 0 {
		t.Errorf("while the command runs: %d tokens locked, holds %v, dead %v; want 2 and the live hold",
			rep.Resources["cpu"].Locked, rep.Holds, rep.DeadHolds)
	}
	e.Start("run", "--no-wait", "--", "true").ExitCode(exitRefusal)
	e.ReleaseChild("closer")
	p.ExitCode(0)
	settled(t, e)
}

// A command that cannot be started (not found, not executable) releases everything at once:
// the run exits 127 or 126 and leaves no hold, live or dead, no ticket and no locked token.
//
// Mutation: the run does not release its grant when the start fails (runChild skips
// c.release on a start error): the tokens go with the process, but the hold is never removed.
func TestFailedSpawnFreesEverything(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(4))
	notExec := filepath.Join(e.MarkDir, "not-executable")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		cmd  string
		code int
	}{
		{"coreslot-no-such-command", 127},
		{filepath.Join(e.MarkDir, "missing"), 127},
		{notExec, 126},
	} {
		label := filepath.Base(c.cmd)
		e.Start("run", "--label", label, "--need", "cpu=2..4", "--", c.cmd).ExitCode(c.code)
		settled(t, e)
		if l := logLine(t, e, label); l["exit"] != float64(c.code) {
			t.Errorf("run-log line %v, want exit %d", l, c.code)
		}
	}
	// Everything is free for the next request.
	e.Start("run", "--no-wait", "--need", "cpu=4", "--", "true").ExitCode(0)
}

// Close closes the process's copies without unlocking, so a command that inherited the grant
// keeps it until it exits, and repeated Close calls return nil and change nothing, even after
// another grant of the same tokens reuses the descriptor numbers.
//
// Mutations: Close releases with LOCK_UN (the command's tokens are freed by the first Close);
// a repeated Close closes again (no closed guard and os.ErrClosed reported): it returns an
// error.
func TestRepeatedClose(t *testing.T) {
	e := State(t)
	e.WriteConfig(cpuConfig(2))
	c := e.Client()
	admit := func(label string) *coreslot.Grant {
		t.Helper()
		w, err := c.Enqueue(context.Background(), coreslot.Request{Label: label, Need: map[string]coreslot.Range{"cpu": {Min: 2, Max: 2}}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), startWait)
		defer cancel()
		g, err := w.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}

	g := admit("lib")
	p := e.startWith(g.Files(), g.Env(), bins.child, "hold", e.MarkDir, "lib-child")
	r := e.WaitStarted("lib-child")
	h, _ := e.Hold("lib")
	checkDescriptors(t, e, r, h.Name, e.HoldBody(h.Name).Grant)
	for i := range 2 {
		if err := g.Close(); err != nil {
			t.Errorf("Close %d: %v", i+1, err)
		}
	}
	if got := cpuLocked(e); got != 2 {
		t.Errorf("%d tokens locked after Close while the command holds them, want 2", got)
	}
	e.ReleaseChild("lib-child")
	p.ExitCode(0)
	e.Eventually(startWait, "the tokens are free once the command exited", func() bool { return cpuLocked(e) == 0 })
	if err := g.Close(); err != nil {
		t.Errorf("Close 3: %v", err)
	}

	// A second grant of the same tokens in this process; the first grant's Close leaves it alone.
	g2 := admit("lib2")
	if err := g.Close(); err != nil {
		t.Errorf("Close 4: %v", err)
	}
	if _, ok := e.Hold("lib2"); !ok || cpuLocked(e) != 2 {
		t.Errorf("the second grant lost its hold or tokens to the first grant's Close (%d locked)", cpuLocked(e))
	}
	for i := range 2 {
		if err := g2.Close(); err != nil {
			t.Errorf("second grant, Close %d: %v", i+1, err)
		}
	}
	// The first grant's hold stayed live while its command held fd 3 (Close found it locked and
	// left it); once the command exited it was dead, and a repeated Close removes nothing.
	e.Reconcile()
	settled(t, e)
}

// ---------------------------------------------------------------- signals

// The signal behavior of `coreslot run` through the built binary, with the helper child as the
// command: under a PTY in terminal mode, SIGTERM and SIGHUP to coreslot's pid, Ctrl-C reaching the
// command exactly once, Ctrl-C while waiting, Ctrl-Z and fg; in non-terminal mode, a foreground
// pipeline interrupted with Ctrl-C or stopped with Ctrl-Z, and SIGINT, SIGQUIT, SIGTERM and SIGHUP
// sent to the wrapper's pid. Every case leaves nothing behind.
//
// Mutation: terminal mode forwards SIGINT to the command too (it is not ignored in terminal
// mode): the command receives Ctrl-C twice.
func TestSignalTraces(t *testing.T) {
	cfg := cpuConfig(2)
	cmdArgs := func(e Env, name string) []string {
		return []string{bins.coreslot, "run", "--label", name, "--need", "cpu=2", "--", bins.child, "signals", e.MarkDir, name}
	}

	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run("terminal/"+unix.SignalName(sig)+" to coreslot", func(t *testing.T) {
			e := State(t)
			e.WriteConfig(cfg)
			p := e.startPTY(nil, cmdArgs(e, "cmd")...)
			r := e.WaitStarted("cmd")
			if pg, _ := unix.Getpgid(r.PID); pg != p.PID() {
				t.Errorf("terminal mode: the command's group is %d, want coreslot's %d", pg, p.PID())
			}
			p.signal(p.PID(), sig)
			p.ExitCode(128 + int(sig))
			if got := received(e, "cmd"); !slices.Equal(got, []string{unix.SignalName(sig)}) {
				t.Errorf("the command received %v, want %v once", got, unix.SignalName(sig))
			}
			settled(t, e)
		})
	}

	t.Run("terminal/Ctrl-C", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(cfg)
		p := e.startPTY(nil, cmdArgs(e, "cmd")...)
		e.WaitStarted("cmd")
		p.write("\x03")
		p.ExitCode(130)
		if got := received(e, "cmd"); !slices.Equal(got, []string{"SIGINT"}) {
			t.Errorf("the command received %v, want SIGINT once", got)
		}
		settled(t, e)
	})

	t.Run("terminal/Ctrl-C while waiting", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(cfg)
		x := e.Run("X", "--need", "cpu=2")
		e.WaitHold("X", startWait)
		p := e.startPTY(nil, cmdArgs(e, "cmd")...)
		e.WaitTicket("cmd")
		p.write("\x03")
		p.ExitCode(130)
		if _, ok := e.Started("cmd"); ok {
			t.Error("the interrupted run's command ran")
		}
		if tk, ok := e.Ticket("cmd"); ok && !tk.Dead {
			t.Error("the interrupted run left a live ticket")
		}
		e.ReleaseChild("X")
		x.ExitCode(0)
		e.Reconcile()
		settled(t, e)
	})

	// Ctrl-Z stops the job, coreslot and its command alike, and fg continues both: in terminal
	// mode (coreslot's stdin is the terminal) and in a pipeline, where the command has its own
	// group that coreslot stops and continues.
	for _, c := range []struct{ name, line string }{
		{"terminal/Ctrl-Z and fg", `"$BIN" run --label cmd --need cpu=2 -- "$CHILD" signals "$M" cmd`},
		{"pipeline/Ctrl-Z and fg", `true | "$BIN" run --label cmd --need cpu=2 -- "$CHILD" signals "$M" cmd | cat`},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := State(t)
			e.WriteConfig(cfg)
			sh := e.shell()
			sh.write(c.line + "\n")
			r := e.WaitStarted("cmd")
			killGroupOf(t, r.PID)
			cs := parentOf(r.PID)
			killGroupOf(t, cs)
			prompts := strings.Count(sh.output(), "csprompt$ ")
			sh.write("\x1a")
			e.Eventually(startWait, "the shell prompting again (the job stopped)", func() bool {
				return strings.Count(sh.output(), "csprompt$ ") > prompts
			})
			e.Eventually(startWait, "coreslot and its command stopped", func() bool {
				return procState(r.PID) == "T" && procState(cs) == "T"
			})
			before := ticks(e, "cmd")
			time.Sleep(300 * time.Millisecond)
			if after := ticks(e, "cmd"); after != before {
				t.Errorf("the command ran while stopped: %d -> %d ticks", before, after)
			}
			sh.write("fg\n")
			e.Eventually(startWait, "the command running again", func() bool { return ticks(e, "cmd") > before+2 })
			if procState(cs) == "T" {
				t.Error("coreslot is still stopped after fg")
			}
			e.ReleaseChild("cmd")
			e.Eventually(startWait, "coreslot exits", func() bool { return procState(cs) == "" })
			if l := logLine(t, e, "cmd"); l["exit"] != float64(0) {
				t.Errorf("run-log line %v, want exit 0", l)
			}
			if got := received(e, "cmd"); len(got) != 0 {
				t.Errorf("the command received %v", got)
			}
			settled(t, e)
		})
	}

	t.Run("pipeline/Ctrl-C", func(t *testing.T) {
		e := State(t)
		e.WriteConfig(cfg)
		sh := e.shell()
		sh.write(`true | "$BIN" run --label cmd --need cpu=2 -- "$CHILD" signals "$M" cmd | cat` + "\n")
		r := e.WaitStarted("cmd")
		killGroupOf(t, r.PID)
		killGroupOf(t, parentOf(r.PID))
		if pg, _ := unix.Getpgid(r.PID); pg != r.PID {
			t.Errorf("non-terminal mode: the command's group is %d, want its own %d", pg, r.PID)
		}
		sh.write("\x03")
		e.Eventually(startWait, "the command exits", func() bool { return procState(r.PID) == "" })
		if got := received(e, "cmd"); !slices.Equal(got, []string{"SIGINT"}) {
			t.Errorf("the command received %v, want SIGINT once", got)
		}
		var l map[string]any
		e.Eventually(startWait, "the run-log line", func() bool {
			if _, err := os.Stat(filepath.Join(e.Dir, store.LogDir, store.RunLog)); err != nil {
				return false
			}
			for _, line := range runLog(t, e) {
				if line["label"] == "cmd" {
					l = line
					return true
				}
			}
			return false
		})
		if l["exit"] != float64(130) {
			t.Errorf("run-log line %v, want exit 130", l)
		}
		settled(t, e)
	})

	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run("non-terminal/"+unix.SignalName(sig)+" to coreslot", func(t *testing.T) {
			e := State(t)
			e.WriteConfig(cfg)
			argv := cmdArgs(e, "cmd")
			p := e.StartCommand(argv...)
			r := e.WaitStarted("cmd")
			killGroupOf(t, r.PID)
			if pg, _ := unix.Getpgid(r.PID); pg != r.PID {
				t.Errorf("non-terminal mode: the command's group is %d, want its own %d", pg, r.PID)
			}
			p.signal(p.PID(), sig)
			p.ExitCode(128 + int(sig))
			if got := received(e, "cmd"); !slices.Equal(got, []string{unix.SignalName(sig)}) {
				t.Errorf("the command received %v, want %v once", got, unix.SignalName(sig))
			}
			settled(t, e)
		})
	}
}

// parentOf is pid's parent, 0 when it cannot be read.
func parentOf(pid int) int {
	var field string
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return 0
		}
		s := string(b)
		if f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:]); len(f) > 1 {
			field = f[1]
		}
	} else if out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output(); err == nil {
		field = strings.TrimSpace(string(out))
	}
	n, _ := strconv.Atoi(field)
	return n
}
