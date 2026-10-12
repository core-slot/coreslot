//go:build unix && coreslot_testhooks

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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
