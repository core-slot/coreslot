package memsample

import (
	"encoding/binary"
	"path"
	"slices"
	"strings"

	"github.com/core-slot/coreslot/internal/testhooks"
)

// VMState says whether Docker Desktop's VM process was found.
type VMState int

const (
	VMNotRunning    VMState = iota // no Docker backend runs, or none of its coalition's processes is the VM
	VMIdentified                   // exactly one VM process in Docker's coalition
	VMNotIdentified                // the table could not be listed, a process of interest could not be read, or more than one candidate matched
)

// VM is Docker Desktop's VM process: the process of one of its VM programs in the resource
// coalition of Docker's backend. PID, StartNS, FootprintMiB and VMM are set only when State is
// VMIdentified.
type VM struct {
	State        VMState
	PID          int
	StartNS      int64
	FootprintMiB int64
	VMM          bool // the VM runs on Docker VMM, which gives freed memory back to the Mac
}

// The executables DockerVM matches, by the last element of their path: Docker's backend, and the VM
// programs of its two backends, Apple's Virtualization framework and Docker VMM. The kernel keeps 16
// bytes of a name in p_comm and 32 in pbi_name, which cut them, so the path is what is compared.
const (
	dockerBackendExe = "com.docker.backend"
	dockerVMMExe     = "com.docker.sailor"
)

var dockerVMExes = []string{"com.apple.Virtualization.VirtualMachine", dockerVMMExe}

// Program is the last element of the identified VM's executable path, the program DockerVM matched,
// which the kernel's process name cuts on Apple's hypervisor.
func (v VM) Program() string {
	if v.VMM {
		return dockerVMMExe
	}
	return dockerVMExes[0]
}

// isVMExe reports whether p runs one of the VM programs.
func isVMExe(p string) bool { return slices.Contains(dockerVMExes, path.Base(p)) }

// coalitionInfoSize is the buffer PROC_PIDCOALITIONINFO (20, not in the public SDK headers) needs:
// uint64 coalition_id[2] (resource, jetsam) and three reserved uint64.
const coalitionInfoSize = 40

// parseCoalition is the resource coalition id in b, the first eight bytes, of a proc_info answer of
// n bytes. Any n other than the full 40 is no coalition: a shorter buffer returns 0 and leaves the
// ids 0, which would put every process in coalition 0.
func parseCoalition(b []byte, n int) (id uint64, ok bool) {
	if n != coalitionInfoSize || len(b) < coalitionInfoSize {
		return 0, false
	}
	return binary.LittleEndian.Uint64(b), true
}

// vmProc is one process of the table DockerVM matches over.
type vmProc struct {
	PID       int
	StartNS   int64
	Exec      string // the executable's full path
	Coalition uint64 // the resource coalition of a process that runs a program DockerVM looks for
	Unread    bool   // a process that may be one of those, whose path or coalition could not be read
}

// mayBeOfInterest reports whether comm, the kernel's 16 bytes of a process's name, may be the start
// of one of the executables DockerVM matches: what a process whose path could not be read is judged by.
func mayBeOfInterest(comm string) bool {
	if len(comm) < 15 {
		return false
	}
	return strings.HasPrefix(dockerBackendExe, comm) || slices.ContainsFunc(dockerVMExes, func(exe string) bool { return strings.HasPrefix(exe, comm) })
}

// exeIs reports whether the last element of p is name.
func exeIs(p, name string) bool { return path.Base(p) == name }

// dockerVM lists the processes once and finds the VM among them: Docker's coalitions are those of the
// processes running com.docker.backend, and the candidates are the processes running either VM
// program in one of them: more than one, of either program or both, is VMNotIdentified. A process that may be one of those and could not be read leaves
// the answer open: VMNotIdentified, never VMNotRunning.
func dockerVM(list func() ([]vmProc, bool), footprint func(pid int) (mib int64, ok bool)) VM {
	procs, ok := list()
	if !ok {
		return VM{State: VMNotIdentified}
	}
	docker := map[uint64]bool{}
	for _, p := range procs {
		if p.Unread {
			return VM{State: VMNotIdentified}
		}
		if exeIs(p.Exec, dockerBackendExe) {
			docker[p.Coalition] = true
		}
	}
	var found []vmProc
	for _, p := range procs {
		if docker[p.Coalition] && isVMExe(p.Exec) {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return VM{State: VMNotRunning}
	case 1:
		mib, ok := footprint(found[0].PID)
		if !ok {
			return VM{State: VMNotRunning} // gone since the listing
		}
		return VM{State: VMIdentified, PID: found[0].PID, StartNS: found[0].StartNS, FootprintMiB: mib, VMM: exeIs(found[0].Exec, dockerVMMExe)}
	}
	return VM{State: VMNotIdentified}
}

// The names the readings file may give DockerVM and VMFootprint, in a test build.
const (
	readingVMState     = "DockerVMState"
	readingVMFootprint = "DockerVMFootprintMiB"
	readingVMPID       = "DockerVMPID"
	readingVMVMM       = "DockerVMVMM"
)

// DockerVM finds Docker Desktop's VM process: the process of one of its VM programs in the
// resource coalition of Docker's backend. It lists the user's processes once and keeps nothing. In
// a test build the readings hook replaces its state (0, 1 or 2) and with it the backend (Docker
// VMM when DockerVMVMM is 1), its footprint and pid, the pid standing for the start time too.
func DockerVM() VM {
	v := lookupVM()
	if nums, set := testhooks.Readings(); set {
		if s, ok := nums[readingVMState]; ok {
			v.State, v.VMM = VMState(s), nums[readingVMVMM] == 1 // the hook's VM, never the host's backend
		}
		if f, ok := nums[readingVMFootprint]; ok {
			v.FootprintMiB = f
		}
		if pid, ok := nums[readingVMPID]; ok {
			v.PID, v.StartNS = int(pid), pid
		}
	}
	return v
}

// VMFootprint is the footprint of the VM process DockerVM identified as pid, started at startNS:
// one read of that process, no listing. ok is false when the process is gone or pid names a process
// with another start time; then the VM is to be looked for again. Off macOS it is always false. In
// a test build the readings hook answers for its DockerVMPID alone, while its state is identified.
func VMFootprint(pid int, startNS int64) (mib int64, ok bool) {
	if nums, set := testhooks.Readings(); set {
		if want, hooked := nums[readingVMPID]; hooked {
			s, stated := nums[readingVMState]
			return nums[readingVMFootprint], int64(pid) == want && startNS == want && (!stated || VMState(s) == VMIdentified)
		}
	}
	return vmFootprint(pid, startNS)
}
