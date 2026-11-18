package memsample

import (
	"encoding/binary"
	"testing"
)

const (
	appleVM = "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine"
	backend = "/Applications/Docker.app/Contents/MacOS/com.docker.backend"
	krun    = "/Applications/Docker.app/Contents/MacOS/com.docker.krun"
)

func proc(pid int, exec string, coalition uint64) vmProc {
	return vmProc{PID: pid, StartNS: int64(pid) * 10, Exec: exec, Coalition: coalition, HasCoalition: true}
}

func fakeTable(procs ...vmProc) func() ([]vmProc, bool) {
	return func() ([]vmProc, bool) { return procs, true }
}

// footprints are 100 MiB times the pid, so a footprint read from the wrong process shows.
func fakeFootprint(pid int) (int64, bool) { return int64(pid) * 100, true }

// The Virtualization VM in the coalition of Docker's backend is identified with its pid, start time
// and footprint. Mutation: returning VMNotRunning, or the footprint of the backend, turns it red.
func TestDockerVMIdentified(t *testing.T) {
	got := dockerVM(fakeTable(proc(5, "/bin/zsh", 9), proc(6, backend, 1147), proc(7, backend, 1147), proc(8, appleVM, 1147)), fakeFootprint)
	if want := (VM{State: VMIdentified, PID: 8, StartNS: 80, FootprintMiB: 800}); got != want {
		t.Errorf("DockerVM = %+v, want %+v", got, want)
	}
}

// A second Virtualization VM in another coalition (a Tart or UTM guest) is not Docker's. Mutation:
// matching the VM by name alone finds two and reports VMNotIdentified.
func TestDockerVMIgnoresVMOutsideDockersCoalition(t *testing.T) {
	got := dockerVM(fakeTable(proc(6, backend, 1147), proc(8, appleVM, 1147), proc(9, appleVM, 2000)), fakeFootprint)
	if got.State != VMIdentified || got.PID != 8 || got.FootprintMiB != 800 {
		t.Errorf("DockerVM = %+v, want pid 8, 800 MiB, identified", got)
	}
}

// Two candidates in Docker's coalition cannot be told apart. Mutation: taking the first candidate
// reports it identified.
func TestDockerVMTwoInCoalitionNotIdentified(t *testing.T) {
	got := dockerVM(fakeTable(proc(6, backend, 1147), proc(8, appleVM, 1147), proc(9, appleVM, 1147)), fakeFootprint)
	if got.State != VMNotIdentified || got.PID != 0 || got.FootprintMiB != 0 {
		t.Errorf("DockerVM = %+v, want VMNotIdentified and nothing else", got)
	}
}

// Without Docker's backend there is no Docker coalition, whatever VMs run. Mutation: treating every
// Virtualization VM as a candidate when no backend runs reports one.
func TestDockerVMNoBackendNotRunning(t *testing.T) {
	if got := dockerVM(fakeTable(proc(8, appleVM, 2000), proc(9, "/bin/zsh", 2000)), fakeFootprint); got != (VM{State: VMNotRunning}) {
		t.Errorf("DockerVM = %+v, want VMNotRunning", got)
	}
	if got := dockerVM(fakeTable(), fakeFootprint); got != (VM{State: VMNotRunning}) {
		t.Errorf("DockerVM of nothing = %+v, want VMNotRunning", got)
	}
}

// Docker's own krun VM is not the Virtualization VM: with it alone, nothing is identified. Mutation:
// adding the krun name back to the candidates identifies it.
func TestDockerVMKrunIsNotRunning(t *testing.T) {
	got := dockerVM(fakeTable(proc(6, backend, 1147), proc(8, krun, 1147)), fakeFootprint)
	if got != (VM{State: VMNotRunning}) {
		t.Errorf("DockerVM = %+v, want VMNotRunning", got)
	}
}

// A proc_info answer shorter than the 40 bytes is no coalition: a 32-byte buffer returns 0 and
// leaves every id 0. Mutation: accepting any positive return reads coalition 0 for the process.
func TestDockerVMShortCoalitionReadIsNoCoalition(t *testing.T) {
	b := make([]byte, coalitionInfoSize)
	binary.LittleEndian.PutUint64(b, 1147)
	if id, ok := parseCoalition(b, 40); !ok || id != 1147 {
		t.Errorf("parseCoalition(40) = %d, %v, want 1147, true", id, ok)
	}
	for _, n := range []int{0, 32, 39, 41} {
		if id, ok := parseCoalition(b, n); ok || id != 0 {
			t.Errorf("parseCoalition(%d) = %d, %v, want no coalition", n, id, ok)
		}
	}
	// A backend and a VM whose coalition read failed are not in a coalition together.
	vm := proc(8, appleVM, 0)
	vm.HasCoalition = false
	be := proc(6, backend, 0)
	be.HasCoalition = false
	if got := dockerVM(fakeTable(be, vm), fakeFootprint); got != (VM{State: VMNotRunning}) {
		t.Errorf("two processes with no coalition matched as coalition 0: %+v", got)
	}
}

// The executable's path decides the name, not the kernel's truncated one: the VM's name is 39 bytes
// and the backend's 18, past the 16 of p_comm. Only the last path element counts. Mutation: matching
// the first 31 bytes of the name, or any path that contains the name, misses the first and takes the
// second.
func TestDockerVMNameFromPathNotComm(t *testing.T) {
	got := dockerVM(fakeTable(proc(6, backend, 1147), proc(8, appleVM, 1147),
		proc(9, "/tmp/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/other", 1147),
		proc(10, "/tmp/com.apple.Virtualization.VirtualMachine.old", 1147)), fakeFootprint)
	if got.State != VMIdentified || got.PID != 8 {
		t.Errorf("DockerVM = %+v, want pid 8 identified", got)
	}
	if got := dockerVM(fakeTable(proc(6, "/x/com.docker.backend2", 1147), proc(8, appleVM, 1147)), fakeFootprint); got.State != VMNotRunning {
		t.Errorf("a longer name matched the backend: %+v", got)
	}
}

// A table that cannot be listed is not an answer. Mutation: reporting VMNotRunning.
func TestDockerVMUnlistableIsNotIdentified(t *testing.T) {
	if got := dockerVM(func() ([]vmProc, bool) { return nil, false }, fakeFootprint); got.State != VMNotIdentified {
		t.Errorf("DockerVM = %+v, want VMNotIdentified", got)
	}
}
