//go:build linux

package config

import (
	"os"
	"os/exec"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

const affinityChildEnv = "CORESLOT_TEST_AFFINITY_CHILD"

// MachineCPUs is the machine-wide online count, so it must not follow a process's affinity mask:
// the test re-runs itself pinned to one CPU and expects the same count there.
func TestMachineCPUs(t *testing.T) {
	data, err := os.ReadFile(onlineCPUs)
	if err != nil {
		t.Fatal(err)
	}
	want, err := parseOnline(string(data))
	if err != nil {
		t.Fatal(err)
	}
	got, err := MachineCPUs()
	if err != nil || got != want {
		t.Fatalf("MachineCPUs() = %d, %v; want %d from %s (runtime.NumCPU() = %d)",
			got, err, want, onlineCPUs, runtime.NumCPU())
	}
	if os.Getenv(affinityChildEnv) != "" {
		if runtime.NumCPU() != 1 {
			t.Fatalf("the one-CPU affinity mask did not apply: runtime.NumCPU() = %d", runtime.NumCPU())
		}
		return
	}
	if want < 2 {
		t.Log("one online CPU: the affinity re-run cannot tell the counts apart; skipped")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMachineCPUs$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), affinityChildEnv+"=1")
	out, err := runPinnedToOneCPU(cmd)
	if err != nil {
		t.Fatalf("under a one-CPU affinity mask: %v\n%s", err, out)
	}
}

// runPinnedToOneCPU starts cmd from a thread pinned to one CPU, so the child inherits that mask.
// The pinned thread is never unlocked: it exits with its goroutine.
func runPinnedToOneCPU(cmd *exec.Cmd) ([]byte, error) {
	type result struct {
		out []byte
		err error
	}
	done := make(chan result)
	go func() {
		runtime.LockOSThread()
		var all, one unix.CPUSet
		if err := unix.SchedGetaffinity(0, &all); err != nil {
			done <- result{nil, err}
			return
		}
		for cpu := 0; cpu < len(all)*64; cpu++ {
			if all.IsSet(cpu) {
				one.Set(cpu)
				break
			}
		}
		if err := unix.SchedSetaffinity(0, &one); err != nil {
			done <- result{nil, err}
			return
		}
		out, err := cmd.CombinedOutput()
		done <- result{out, err}
	}()
	r := <-done
	return r.out, r.err
}
