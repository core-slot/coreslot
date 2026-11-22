package memsample

import (
	"encoding/binary"
	"testing"
)

const (
	appleVM = "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine"
	backend = "/Applications/Docker.app/Contents/MacOS/com.docker.backend"
	krun    = "/Applications/Docker.app/Contents/MacOS/com.docker.krun"
	sailor  = "/Applications/Docker.app/Contents/MacOS/com.docker.sailor"
)

func proc(pid int, exec string, coalition uint64) vmProc {
	return vmProc{PID: pid, StartNS: int64(pid) * 10, Exec: exec, Coalition: coalition}
}

func fakeTable(procs ...vmProc) func() ([]vmProc, bool) {
	return func() ([]vmProc, bool) { return procs, true }
}

// footprints are 100 MiB times the pid, so a footprint read from the wrong process shows.
func fakeFootprint(pid int) (int64, bool) { return int64(pid) * 100, true }

// The Virtualization VM in the coalition of Docker's backend is identified with its pid, start time
// and footprint, not as Docker VMM. Mutation: returning VMNotRunning, the footprint of the backend,
// or VMM for every VM program turns it red.
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
}

// A process of interest that could not be read leaves the answer open: VMNotIdentified, never
// VMNotRunning, so status still reads the engine. Mutations: skipping an Unread process gives
// VMNotRunning in rows 1 to 3 and VMIdentified in the last; coalition 0 for both in row 1 matches them.
func TestDockerVMUnreadProcessIsNotIdentified(t *testing.T) {
	lost := func(p vmProc) vmProc { p.Unread = true; return p }
	for _, c := range []struct {
		name  string
		procs []vmProc
	}{
		{"backend and VM both lost their coalition", []vmProc{lost(proc(6, backend, 0)), lost(proc(8, appleVM, 0))}},
		{"backend lost its coalition", []vmProc{lost(proc(6, backend, 0)), proc(8, appleVM, 1147)}},
		{"backend's path was unreadable", []vmProc{{PID: 6, Unread: true}}},
		{"a VM candidate lost its coalition", []vmProc{proc(6, backend, 1147), proc(8, appleVM, 1147), lost(proc(9, appleVM, 0))}},
	} {
		if got := dockerVM(fakeTable(c.procs...), fakeFootprint); got != (VM{State: VMNotIdentified}) {
			t.Errorf("%s: DockerVM = %+v, want VMNotIdentified", c.name, got)
		}
	}
}

// The kernel's 16 bytes of a name say whether an unreadable process may be one DockerVM looks for.
// Mutation: a prefix test either way round, or none, reddens a row.
func TestMayBeOfInterest(t *testing.T) {
	for comm, want := range map[string]bool{"com.docker.backe": true, "com.docker.back": true, "com.apple.Virtua": true,
		"com.docker.krun": false, "com": false, "": false, "zsh": false, "com.docker.backend2": false} {
		if got := mayBeOfInterest(comm); got != want {
			t.Errorf("mayBeOfInterest(%q) = %v, want %v", comm, got, want)
		}
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

// Docker VMM's VM, com.docker.sailor, in the coalition of Docker's backend is identified as the
// Apple backend's VM is, as Docker VMM. Mutation: dropping it from the VM programs gives
// VMNotRunning; VMM never set gives false.
func TestDockerVMFindsSailor(t *testing.T) {
	got := dockerVM(fakeTable(proc(6, backend, 1147), proc(7, krun, 1147), proc(8, sailor, 1147)), fakeFootprint)
	if want := (VM{State: VMIdentified, PID: 8, StartNS: 80, FootprintMiB: 800, VMM: true}); got != want {
		t.Errorf("DockerVM = %+v, want %+v", got, want)
	}
}

// One VM of each program in Docker's coalition is two candidates: not identified. Mutation: looking
// for the programs one after the other, the first found winning, identifies one.
func TestDockerVMOneOfEachNotIdentified(t *testing.T) {
	got := dockerVM(fakeTable(proc(6, backend, 1147), proc(8, appleVM, 1147), proc(9, sailor, 1147)), fakeFootprint)
	if got != (VM{State: VMNotIdentified}) {
		t.Errorf("DockerVM = %+v, want VMNotIdentified", got)
	}
}

// A com.docker.sailor outside Docker's coalition is not Docker's VM. Mutation: matching the sailor
// by name alone identifies it.
func TestSailorOutsideDockerCoalitionNotRunning(t *testing.T) {
	got := dockerVM(fakeTable(proc(6, backend, 1147), proc(9, sailor, 2000)), fakeFootprint)
	if got != (VM{State: VMNotRunning}) {
		t.Errorf("DockerVM = %+v, want VMNotRunning", got)
	}
}

// The kernel keeps 16 bytes of com.docker.sailor, com.docker.sailo: a process of that name whose
// path cannot be read may be Docker VMM's VM, which leaves the answer open. Mutation: leaving the
// sailor out of mayBeOfInterest makes it false.
func TestMayBeOfInterestSailor(t *testing.T) {
	if !mayBeOfInterest("com.docker.sailo") {
		t.Error(`mayBeOfInterest("com.docker.sailo") = false, want true`)
	}
}
