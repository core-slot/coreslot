//go:build unix && coreslot_testhooks

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// pausePoint is the handshake prefix of the pause point name of process pid under hooks.
func pausePoint(hooks string, pid int, name string) string {
	return filepath.Join(hooks, fmt.Sprintf("%d.%s", pid, name))
}

func reached(prefix string) func() bool {
	return func() bool { _, err := os.Stat(prefix + ".reached"); return err == nil }
}

func resume(t *testing.T, prefix string) {
	t.Helper()
	if err := os.WriteFile(prefix+".go", nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// watchdog kills pid's process group after a bound of its own: a test binary killed by its
// timeout runs no cleanup, and a process paused at a point waits for ever.
func watchdog(t *testing.T, pid int) {
	t.Helper()
	w := exec.Command("/bin/sh", "-c", fmt.Sprintf("sleep 120; kill -KILL -- -%d 2>/dev/null", pid))
	w.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(-w.Process.Pid, unix.SIGKILL); _ = w.Wait() })
}

// finish lets cmd run to its end, releasing every pause at prefix it reaches meanwhile, and
// returns its status.
func finish(t *testing.T, cmd *exec.Cmd, prefix string) int {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("the run did not finish within 60 s (load: %s)", uptime())
		case <-done:
			return exitStatus(cmd.ProcessState)
		case <-time.After(20 * time.Millisecond):
			if reached(prefix)() {
				resume(t, prefix)
			}
		}
	}
}

// A run that waits to enter the admission section, which another client holds, stops at once on
// SIGTSTP: only holding the section defers a stop, waiting for it must not, or Ctrl-Z would do
// nothing for as long as that client (stopped itself, say) keeps the section. The test process
// holds admit.lock; the run's enqueue pauses at its first busy try and is sent SIGTSTP there.
// Mutation: call the lock hook's enter before Enter's loop and its leave only on return, so
// that it brackets the wait as well (the run does not stop while the section stays held).
func TestStopWhileWaitingForTheSection(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 2\n")
	if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "admit.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	hooks := t.TempDir()
	cmd := cliCmd(t, append(baseEnv(dir), "CORESLOT_TESTHOOK_DIR="+hooks, "CORESLOT_TESTHOOK_PAUSE=section.busy"),
		"run", "--", "true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	watchdog(t, cmd.Process.Pid)
	busy := pausePoint(hooks, cmd.Process.Pid, "section.busy")
	waitFor(t, 30*time.Second, "the run waiting for the section", reached(busy))
	if err := unix.Kill(cmd.Process.Pid, unix.SIGTSTP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the waiting run stopping", func() bool { return procState(t, cmd.Process.Pid) == "T" })

	// Continued, and with the section free, the run goes on as if nothing had happened.
	if err := unix.Kill(cmd.Process.Pid, unix.SIGCONT); err != nil {
		t.Fatal(err)
	}
	resume(t, busy)
	lock.Close()
	if code := finish(t, cmd, busy); code != 0 {
		t.Errorf("the run exited %d, want 0", code)
	}
}

// A run that holds the admission section defers SIGTSTP until it has left the section, and then
// stops: a client stopped inside the section blocks every admission on the machine (PROTOCOL.md
// section 8). The run pauses inside the section of its enqueue and is sent SIGTSTP there; when
// it goes on, it must stop with the section already free.
// Mutations: Enter no longer calls the lock hook (the run stops inside the section); Leave no
// longer calls it (the run never stops).
func TestStopDeferredWhileHoldingTheSection(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 2\n")
	if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	hooks := t.TempDir()
	cmd := cliCmd(t, append(baseEnv(dir), "CORESLOT_TESTHOOK_DIR="+hooks, "CORESLOT_TESTHOOK_PAUSE=section.entered"),
		"run", "--", "true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	watchdog(t, cmd.Process.Pid)
	entered := pausePoint(hooks, cmd.Process.Pid, "section.entered")
	waitFor(t, 30*time.Second, "the run's pause inside the section", reached(entered))
	if !sectionLocked(t, dir) {
		t.Fatal("the paused run does not hold the admission section")
	}
	if err := unix.Kill(cmd.Process.Pid, unix.SIGTSTP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if st := procState(t, cmd.Process.Pid); st == "T" {
		t.Fatal("the run stopped inside the admission section")
	}
	resume(t, entered)
	waitFor(t, 10*time.Second, "the deferred stop once the section is left", func() bool {
		return procState(t, cmd.Process.Pid) == "T"
	})
	if sectionLocked(t, dir) {
		t.Error("the run stopped before it left the admission section")
	}
	if err := unix.Kill(cmd.Process.Pid, unix.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if code := finish(t, cmd, entered); code != 0 {
		t.Errorf("the run exited %d, want 0", code)
	}
}

// A status that holds a token's probe lock defers SIGTSTP until it has closed the probe, and then
// stops: a client stopped holding a probe lock makes every other client count the token as
// locked, so a head whose minimum needs it is never admitted although nobody is stopped inside
// the admission section (I5). The status pauses in its probe of cpu's first token, holding its
// lock, and is sent SIGTSTP there; once it goes on, it must stop with that token free, and a
// request for the whole of cpu must be admitted at once while it stays stopped.
// Mutation: ProbeLocked no longer calls the lock hooks (the status stops holding the token).
func TestStopDeferredWhileHoldingAProbeLock(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 2\n")
	if r := cli(t, baseEnv(dir), "run", "--need", "cpu=2", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	whole := func() result {
		return cli(t, baseEnv(dir), "run", "--no-wait", "--need", "cpu=2", "--", "true")
	}
	hooks := t.TempDir()
	cmd := cliCmd(t, append(baseEnv(dir), "CORESLOT_TESTHOOK_DIR="+hooks, "CORESLOT_TESTHOOK_PAUSE=probe.locked"),
		"status")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	watchdog(t, cmd.Process.Pid)
	probing := pausePoint(hooks, cmd.Process.Pid, "probe.locked")
	waitFor(t, 30*time.Second, "the status's pause inside a token probe", reached(probing))
	if r := whole(); r.code == 0 {
		t.Fatal("the paused status does not hold a token's probe lock")
	}
	if err := unix.Kill(cmd.Process.Pid, unix.SIGTSTP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if st := procState(t, cmd.Process.Pid); st == "T" {
		r := whole()
		t.Fatalf("the status stopped holding a token's probe lock; a request for the whole of cpu then exits %d: %q",
			r.code, r.stderr)
	}
	resume(t, probing)
	waitFor(t, 10*time.Second, "the deferred stop once the probe is closed", func() bool {
		return procState(t, cmd.Process.Pid) == "T"
	})
	if r := whole(); r.code != 0 {
		t.Errorf("while the status is stopped, a request for the whole of cpu exits %d, want 0: %q", r.code, r.stderr)
	}
	if err := unix.Kill(cmd.Process.Pid, unix.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if code := finish(t, cmd, probing); code != 0 {
		t.Errorf("the status exited %d, want 0", code)
	}
}
