package memsample

import (
	"encoding/binary"
	"path"
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

// VM is Docker Desktop's VM process: the Virtualization VM in the resource coalition of Docker's
// backend. PID, StartNS and FootprintMiB are set only when State is VMIdentified.
type VM struct {
	State        VMState
	PID          int
	StartNS      int64
	FootprintMiB int64
}

// The executables DockerVM matches, by the last element of their path. The kernel keeps 16 bytes of
// a name in p_comm and 32 in pbi_name, which cut both, so the path is what is compared.
const (
	dockerBackendExe = "com.docker.backend"
	dockerVMExe      = "com.apple.Virtualization.VirtualMachine"
)

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
	return len(comm) >= 15 && (strings.HasPrefix(dockerBackendExe, comm) || strings.HasPrefix(dockerVMExe, comm))
}

// exeIs reports whether the last element of p is name.
func exeIs(p, name string) bool { return path.Base(p) == name }

// dockerVM lists the processes once and finds the VM among them: Docker's coalitions are those of the
// processes running com.docker.backend, and the candidates are the processes running the
// Virtualization VM in one of them. A process that may be one of those and could not be read leaves
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
		if docker[p.Coalition] && exeIs(p.Exec, dockerVMExe) {
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
		return VM{State: VMIdentified, PID: found[0].PID, StartNS: found[0].StartNS, FootprintMiB: mib}
	}
	return VM{State: VMNotIdentified}
}

// The names the readings file may give DockerVM, in a test build.
const (
	readingVMState     = "DockerVMState"
	readingVMFootprint = "DockerVMFootprintMiB"
)

// DockerVM finds Docker Desktop's VM process: the Virtualization VM in the resource coalition of
// Docker's backend. It lists the user's processes once and keeps nothing. In a test build the
// readings hook replaces its state (0, 1 or 2) and footprint.
func DockerVM() VM {
	v := lookupVM()
	if nums, set := testhooks.Readings(); set {
		if s, ok := nums[readingVMState]; ok {
			v.State = VMState(s)
		}
		if f, ok := nums[readingVMFootprint]; ok {
			v.FootprintMiB = f
		}
	}
	return v
}
