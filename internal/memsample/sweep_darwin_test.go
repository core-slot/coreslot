package memsample

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The sweep finds this process, by the name proc_info gives, with a footprint, a resident size,
// CPU time and its start time. n larger than the process count gives every readable one.
// Mutation: skipping a process whose rusage reads zero CPU time is harmless, but dropping the
// uid filter's own uid, or reading comm of the short flavor, loses this binary's name.
func TestSweepFindsThisProcess(t *testing.T) {
	procs, ok := Listing()
	if !ok {
		t.Fatal("no listing")
	}
	top, _, ok := Sweep(1 << 20)
	if !ok || len(top) == 0 || len(top) > len(procs) {
		t.Fatalf("Sweep: %d entries of %d processes, ok %v", len(top), len(procs), ok)
	}
	self := procs[os.Getpid()]
	var found *ProcMem
	for i := range top {
		if top[i].StartNS == self.StartNS && top[i].Name == filepath.Base(os.Args[0]) {
			found = &top[i]
		}
	}
	if found == nil {
		t.Fatalf("this process (%q, start %d) is not among %d entries", filepath.Base(os.Args[0]), self.StartNS, len(top))
	}
	if found.FootprintMiB <= 0 || found.ResidentMiB <= 0 || found.CPUS <= 0 {
		t.Errorf("this process reads as %+v", *found)
	}
}

// A process whose name is longer than the 15 characters of the short flavor is found as the VM by
// the long name: a copy of the test binary named like the VM's helper, run as the helper. Mutation:
// reading the short info flavor (PROC_PIDT_SHORTBSDINFO) names it "com.apple.Virtual", not a VM.
func TestSweepReadsLongName(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const name = "com.apple.Virtualization.VirtualMachine"
	copyPath := filepath.Join(t.TempDir(), name)
	src, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, src, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(copyPath)
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, _ := bufio.NewReader(io.LimitReader(out, 64)).ReadString('\n'); line != "ready\n" {
		t.Fatalf("helper said %q", line)
	}
	procs, _ := Listing()
	_, vm, ok := sweepPIDs([]procRef{{cmd.Process.Pid, procs[cmd.Process.Pid].StartNS}}, 5)
	if !ok || vm == nil || vm.Name != name[:31] || vm.FootprintMiB <= 0 {
		t.Errorf("vm = %+v (ok %v), want %q with a footprint", vm, ok, name[:31])
	}
}

// A sweep costs a few ms; BenchmarkSweep prints how many.
func BenchmarkSweep(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, _, ok := Sweep(5); !ok {
			b.Fatal("not ok")
		}
	}
}
