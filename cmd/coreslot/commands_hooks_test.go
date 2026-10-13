//go:build unix && coreslot_testhooks

package main

import (
	"bytes"
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

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// formatsOfThisBuild is the formats the CLI of this test build implements: with the test hooks
// the environment decides, and the default is format 1.
func formatsOfThisBuild() string {
	if v := os.Getenv("CORESLOT_TESTHOOK_FORMATS"); v != "" {
		return strings.ReplaceAll(v, ",", ", ")
	}
	return "1"
}

func TestVersionListsEveryImplementedFormat(t *testing.T) {
	env := append(baseEnv(stateDir(t, "")), "CORESLOT_TESTHOOK_FORMATS=1,2")
	r := cli(t, env, "version")
	if r.code != 0 || r.stdout != "coreslot "+cliVersion()+" (protocol formats: 1, 2)\n" {
		t.Errorf("version: exit %d, stdout %q", r.code, r.stdout)
	}
}

// A real client paused inside the section: status names its pid. FORMAT exists already, so the
// first point is the ticket's publication, after the entry wrote admit.lock's body.
func TestStatusNamesSectionHolderOfPausedRun(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	hooks := t.TempDir()
	env := append(baseEnv(dir), "CORESLOT_TESTHOOK_DIR="+hooks, "CORESLOT_TESTHOOK_PAUSE=publish.after-create")
	cmd := cliCmd(t, env, "run", "--", "true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// cliCmd kills the run's process group when the test ends by returning or failing. A test
	// binary killed by its timeout runs no cleanup, and a run paused at a point waits for ever,
	// so a watchdog outside the test binary kills the group after a bound of its own.
	watchdog := exec.Command("/bin/sh", "-c", fmt.Sprintf("sleep 90; kill -KILL -- -%d 2>/dev/null", cmd.Process.Pid))
	watchdog.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := watchdog.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(-watchdog.Process.Pid, unix.SIGKILL); _ = watchdog.Wait() })
	reached := filepath.Join(hooks, fmt.Sprintf("%d.publish.after-create.reached", cmd.Process.Pid))
	waitFor(t, 30*time.Second, "the run's pause inside the section", func() bool {
		_, err := os.Stat(reached)
		return err == nil
	})
	r := cli(t, baseEnv(dir), "status")
	want := fmt.Sprintf("admission section: held by pid %d for ", cmd.Process.Pid)
	if r.code != 0 || !strings.Contains(r.stdout, want) {
		t.Errorf("status exited %d, want %q in:\n%s", r.code, want, r.stdout)
	}
	// The run reaches the point again for its hold; release it every time until it exits.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("the paused run did not finish within 60 s of being released (load: %s)", uptime())
		case err := <-done:
			if err != nil {
				t.Errorf("the paused run: %v", err)
			}
			return
		case <-time.After(20 * time.Millisecond):
			if _, err := os.Stat(reached); err == nil {
				if err := os.WriteFile(strings.TrimSuffix(reached, ".reached")+".go", nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// cancel that entered the section in time and then met a state error reports that error, not a
// section that stayed held: the bound covers the entry alone. The cancel pauses right after its
// entry until its bound has passed, and queue/ cannot be read when it goes on.
func TestCancelStateErrorAfterTimelyEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 entry")
	}
	dir := stateDir(t, "")
	if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	hooks := t.TempDir()
	env := append(baseEnv(dir), "CORESLOT_TESTHOOK_DIR="+hooks, "CORESLOT_TESTHOOK_PAUSE=section.entered")
	cmd := cliCmd(t, env, "cancel", wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	watchdog := exec.Command("/bin/sh", "-c", fmt.Sprintf("sleep 90; kill -KILL -- -%d 2>/dev/null", cmd.Process.Pid))
	watchdog.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := watchdog.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(-watchdog.Process.Pid, unix.SIGKILL); _ = watchdog.Wait() })
	reached := filepath.Join(hooks, fmt.Sprintf("%d.section.entered.reached", cmd.Process.Pid))
	waitFor(t, 30*time.Second, "cancel's pause inside the section", func() bool {
		_, err := os.Stat(reached)
		return err == nil
	})
	time.Sleep(cancelWait + 500*time.Millisecond) // the bound has passed, inside the section
	queue := filepath.Join(dir, "queue")
	if err := os.Chmod(queue, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(queue, 0o700) })
	if err := os.WriteFile(strings.TrimSuffix(reached, ".reached")+".go", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if code := exitStatus(cmd.ProcessState); code != 75 || strings.Contains(stderr.String(), "stayed held") ||
		!strings.HasPrefix(stderr.String(), "coreslot: cancel refused: open ") || !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("cancel: exit %d, want 75 refusing with the I/O error, not a busy section (stderr %q)", code, stderr.String())
	}
}

// probeScript writes an executable script for CORESLOT_TESTHOOK_PROBE_SHELL, which the run starts
// with no environment, and returns its path.
func probeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// probeFallback is the soft limit a run started with a soft limit of 256 gives its command when
// its probe fails: 256 above the limit the Go runtime raised the run's own to, within the hard
// limit and kern.maxfilesperproc.
func probeFallback(t *testing.T) uint64 {
	t.Helper()
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	hard := uint64(lim.Max)
	if hard < 1024 {
		t.Skipf("the hard limit %d leaves no room above 256 + 256", hard)
	}
	raised := hard - 1
	if c := fileCap(); c > 0 {
		raised = min(raised, c)
	}
	// min(raised + 256, hard), without overflowing when the hard limit is unlimited.
	want := min(raised, hard-extraNofile) + extraNofile
	if c := fileCap(); c > 0 {
		want = min(want, c)
	}
	return want
}

// A probe that fails never fails a run: the command runs with 256 above the limit the Go runtime
// raised the run's own to, as before the probe. A probe that does not end is killed at its bound,
// and one still running when the run refuses is killed with the run; neither outlives it. The
// sleeping probe ends on its own after 30 s, so a run that waited for it instead takes 30 s.
// Mutations: make runChild return 70 when the probe failed (every case goes red); drop the
// deferred stop in runCommand (the refusing run leaves its probe sleeping); start the probe
// with context.WithCancel instead of context.WithTimeout (no end takes 30 s).
func TestRunFallsBackWhenTheProbeFails(t *testing.T) {
	want := probeFallback(t)
	dir := stateDir(t, "[resources]\ncpu = 1\n")
	pids := t.TempDir()
	sleeper := func(name string) string {
		return probeScript(t, fmt.Sprintf("echo $$ > %q\nexec /bin/sleep 30\n", filepath.Join(pids, name)))
	}
	// gone fails the test when the probe that wrote pids/name is still alive.
	gone := func(t *testing.T, name string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(pids, name))
		if err != nil {
			t.Fatalf("the probe never ran: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
			_ = unix.Kill(pid, unix.SIGKILL)
			t.Errorf("the probe %d outlived the run (kill 0: %v)", pid, err)
		}
	}

	for _, tc := range []struct{ name, shell string }{
		{"missing", filepath.Join(t.TempDir(), "missing")},
		{"unparsable", probeScript(t, "echo many\n")},
		{"no end", sleeper("timeout")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := append(baseEnv(dir), "CORESLOT_TESTHOOK_PROBE_SHELL="+tc.shell)
			cmd := shCmd(t, env, `ulimit -S -n 256 && exec "$0" run -- sh -c 'ulimit -S -n'`, cliBinary(t))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			start := time.Now()
			out, err := cmd.Output()
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("%v (stderr %q)", err, stderr.String())
			}
			if got := strings.TrimSpace(string(out)); got != strconv.FormatUint(want, 10) {
				t.Errorf("the command's soft limit is %s, want the fallback %d", got, want)
			}
			if tc.name == "no end" {
				if elapsed > 10*time.Second {
					t.Errorf("the run took %v; want about the probe's 1 s bound, not the probe's 30 s", elapsed)
				}
				gone(t, "timeout")
			}
		})
	}

	// The run refuses only once its probe is known to run: the probe's own bound is lifted, so
	// nothing but the run's stop can end it before its 30 s sleep, and the refusal is a cancel
	// sent after the probe wrote its pid and the run's ticket is queued. A refusal on a
	// deadline instead would race the probe's start (a probe that has not written its pid by
	// then fails the test) or, past the probe's 1 s bound, be preceded by that bound's kill,
	// which would hide a missing stop.
	t.Run("refused", func(t *testing.T) {
		holdToken(t, dir, "cpu", 0)
		pidFile := filepath.Join(pids, "refused")
		env := append(baseEnv(dir), "CORESLOT_TESTHOOK_PROBE_SHELL="+sleeper("refused"), "CORESLOT_TESTHOOK_PROBE_TIMEOUT=10m")
		cmd := shCmd(t, env, `ulimit -S -n 256 && exec "$0" run --label refused -- true`, cliBinary(t))
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 30*time.Second, "the probe's pid and the run's ticket", func() bool {
			b, err := os.ReadFile(pidFile)
			if err != nil {
				return false
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			return err == nil && unix.Kill(pid, 0) == nil && len(entries(t, dir, "queue")) == 1
		})
		if r := cli(t, baseEnv(dir), "cancel", "--label", "refused"); r.code != 0 {
			t.Fatalf("cancel: exit %d (stderr %q)", r.code, r.stderr)
		}
		_ = cmd.Wait()
		if code := exitStatus(cmd.ProcessState); code != 75 || !strings.Contains(out.String(), "the request was cancelled") {
			t.Fatalf("exit %d, want 75 (the request was cancelled):\n%s", code, out.String())
		}
		gone(t, "refused")
	})
}

// The probe never inherits a grant's descriptors. A pass-through's inherited grant is
// close-on-exec from the start of the process until its command starts, so its command still
// inherits the grant and its probe does not.
//
// Each probe reports through the test binary, which is slower than the probe's 1 s bound allows
// in a race-enabled build: the race runtime sleeps 1 s before the program exits. So the test
// lifts the bound through CORESLOT_TESTHOOK_PROBE_TIMEOUT, and each report ends with a line the
// probe writes only once the helper has exited; a probe killed at its bound would leave an
// unfinished report, and on Linux the pass-through would not probe at all, since the fallback
// starts it at its hard limit, which the runtime does not raise.
//
// Mutations: drop the shieldInherited call from main, and the nested probe reports the parent's
// hold and token; skip the restoring loop before start in exposeInherited, and the nested command
// inherits neither; drop CORESLOT_TESTHOOK_PROBE_TIMEOUT from the test, and a race-enabled run
// goes red.
func TestNofileProbeInheritsNoGrant(t *testing.T) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	if uint64(lim.Max) < 1024 {
		t.Skipf("the hard limit %d leaves no room above 256 + 256", lim.Max)
	}
	dir := stateDir(t, "[resources]\ncpu = 1\n")
	reports := t.TempDir()
	probe := probeScript(t, fmt.Sprintf("%s=fds %q > %q/probe.$$ && echo reported >> %q/probe.$$\nulimit -S -n\n",
		helperEnv, cliBinary(t), reports, reports))
	env := append(baseEnv(dir), "CORESLOT_TESTHOOK_PROBE_SHELL="+probe, "CORESLOT_TESTHOOK_PROBE_TIMEOUT=2m")
	cmd := shCmd(t, env, `ulimit -S -n 256 && exec "$0" run -- "$0" run -- env `+helperEnv+`=fds "$0"`, cliBinary(t))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	// files is the (device, inode) of each descriptor an fds report lists, or of those at the
	// numbers in only when it names any. A report also lists the reporting helper's own runtime
	// descriptors, such as its poller.
	files := func(report string, only ...string) []string {
		var ids []string
		for _, l := range strings.Split(report, "\n") {
			if f := strings.Fields(l); len(f) == 4 && f[0] == "fd" && (len(only) == 0 || slices.Contains(only, f[1])) {
				ids = append(ids, f[2]+" "+f[3])
			}
		}
		return ids
	}
	// The grant of cpu = 1 is the hold at fd 3 and the token at fd 4.
	grant := files(string(out), "3", "4")
	if !strings.Contains(string(out), "env CORESLOT_HELD_FD=3\n") || !strings.Contains(string(out), "env CORESLOT_TOKEN_FDS=4\n") ||
		len(grant) != 2 {
		t.Fatalf("the nested command did not inherit the hold at fd 3 and the token at fd 4:\n%s", out)
	}
	des, err := os.ReadDir(reports)
	if err != nil {
		t.Fatal(err)
	}
	if len(des) != 2 {
		t.Fatalf("%d probe reports, want the outer run's and the pass-through's", len(des))
	}
	for _, de := range des {
		b, err := os.ReadFile(filepath.Join(reports, de.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(string(b), "reported\n") {
			t.Errorf("a probe ended before its report was complete:\n%s", b)
		}
		for _, id := range files(string(b)) {
			if slices.Contains(grant, id) {
				t.Errorf("a probe inherited a grant descriptor (%s):\n%s", id, b)
			}
		}
	}
}

// A run's descriptor-limit probe has exec'd before the run publishes anything. Until the forked
// child execs it shares every descriptor of the run, close-on-exec ones included, so a probe
// forked while the run held its ticket kept that ticket locked after the run closed it, and the
// run's own probe at admission then left the dead ticket in queue/ for a reconciliation instead
// of unlinking it (PROTOCOL.md section 12.1 step 7). The run pauses just before it starts the
// probe, and it must publish nothing while it is paused.
// Mutation: start the probe inside startNofileProbe's goroutine again (pause point included), and
// the paused run publishes its hold.
func TestNofileProbeExecsBeforeThePublication(t *testing.T) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	if uint64(lim.Max) < 1024 {
		t.Skipf("the hard limit %d leaves no room above 256 + 256", lim.Max)
	}
	dir := stateDir(t, "[resources]\ncpu = 1\n")
	hooks := t.TempDir()
	env := append(baseEnv(dir), "CORESLOT_TESTHOOK_DIR="+hooks, "CORESLOT_TESTHOOK_PAUSE=probe.before-start")
	cmd := shCmd(t, env, `ulimit -S -n 256 && exec "$0" run --no-wait -- true`, cliBinary(t))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	reached := filepath.Join(hooks, fmt.Sprintf("%d.probe.before-start.reached", cmd.Process.Pid))
	waitFor(t, 30*time.Second, "the run's pause before its probe starts", func() bool {
		_, err := os.Stat(reached)
		return err == nil
	})
	published := func() []string {
		var names []string
		for _, d := range []string{"queue", "holds"} {
			des, _ := os.ReadDir(filepath.Join(dir, d))
			for _, de := range des {
				names = append(names, d+"/"+de.Name())
			}
		}
		return names
	}
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if names := published(); len(names) != 0 {
			t.Fatalf("the run published %v before its probe started", names)
		}
	}
	if err := os.WriteFile(strings.TrimSuffix(reached, ".reached")+".go", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the run: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the released run did not finish within 60 s (load: %s)", uptime())
	}
	if names := published(); len(names) != 0 {
		t.Errorf("the run left %v behind", names)
	}
}
