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

// A process whose executable is named like the VM's is found by the last element of its path, in the
// full 39 bytes, where its names (kept to 16 in p_comm and 32 in pbi_name) match nothing: a copy of
// the test binary named like the VM's helper, run as the helper. With a backend in its real
// coalition it is Docker's VM, at its real footprint. Mutations: reading the path flavor into a
// buffer that is too short, or listVMProcs taking the name from procName, loses the match; a
// coalition read into the wrong bytes, or a footprint read of another pid, reddens the last check.
func TestListVMProcsReadsPathNotName(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), dockerVMExes[0])
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
	procs, ok := listVMProcs()
	if !ok {
		t.Fatal("no listing")
	}
	var found *vmProc
	for i := range procs {
		if procs[i].PID == cmd.Process.Pid {
			found = &procs[i]
		}
	}
	if found == nil || !exeIs(found.Exec, dockerVMExes[0]) {
		t.Fatalf("the helper is %+v, want an executable named %s", found, dockerVMExes[0])
	}
	if name, _ := procName(found.PID); name == dockerVMExes[0] {
		t.Errorf("procName kept all %d bytes of the name; the test no longer shows the truncation", len(name))
	}
	be := vmProc{PID: os.Getpid(), Exec: "/x/" + dockerBackendExe, Coalition: found.Coalition}
	got := dockerVM(func() ([]vmProc, bool) { return []vmProc{be, *found}, true }, footprintMiB)
	if got.State != VMIdentified || got.PID != found.PID || got.StartNS != found.StartNS || got.FootprintMiB <= 0 {
		t.Errorf("DockerVM = %+v, want the helper (pid %d), identified, with a footprint", got, found.PID)
	}
}

// Sweep's vm is DockerVM's process: read when identified, absent when not identified or not running,
// and named by its backend's program in full. Mutations: dropping the DockerVM wiring in Sweep, or
// reading vm whatever the state, reddens a row; keeping the kernel's process name names it after this
// test binary.
func TestSweepVMComesFromDockerVM(t *testing.T) {
	procs, ok := Listing()
	if !ok {
		t.Fatal("no listing")
	}
	self := VM{State: VMIdentified, PID: os.Getpid(), StartNS: procs[os.Getpid()].StartNS, FootprintMiB: 1}
	saved := lookupVM
	t.Cleanup(func() { lookupVM = saved })
	for _, c := range []struct {
		state VMState
		vmm   bool
		name  string
	}{
		{VMIdentified, false, "com.apple.Virtualization.VirtualMachine"}, {VMIdentified, true, "com.docker.sailor"},
		{VMNotIdentified, false, ""}, {VMNotRunning, false, ""},
	} {
		lookupVM = func() VM { v := self; v.State, v.VMM = c.state, c.vmm; return v }
		_, vm, ok := Sweep(1)
		if want := c.state == VMIdentified; !ok || (vm != nil) != want {
			t.Errorf("state %d: Sweep's vm = %+v (ok %v), want present %v", c.state, vm, ok, want)
		}
		if vm != nil && (vm.StartNS != self.StartNS || vm.FootprintMiB <= 0 || vm.Name != c.name) {
			t.Errorf("vm = %+v, want this process's start time, a footprint and the name %s", *vm, c.name)
		}
	}
}

// A process has a resource coalition of its own, read in the full 40 bytes; the same read into 32
// returns 0 and no id. Mutation: a coalition buffer of 32 bytes reports no coalition here.
func TestCoalitionReadOfThisProcess(t *testing.T) {
	b := make([]byte, coalitionInfoSize)
	n, err := info(callPidInfo, os.Getpid(), pidCoalitionInfo, 0, b)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := parseCoalition(b, n); !ok || id == 0 {
		t.Errorf("coalition of this process = %d, %v (read %d bytes), want an id", id, ok, n)
	}
	if n, _ := info(callPidInfo, os.Getpid(), pidCoalitionInfo, 0, b[:32]); n == coalitionInfoSize {
		t.Errorf("a 32-byte buffer read %d bytes", n)
	}
}
