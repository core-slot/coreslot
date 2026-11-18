package memsample

import (
	"encoding/binary"
	"strings"

	"github.com/core-slot/coreslot/internal/testhooks"
)

// VMState says whether Docker Desktop's VM process was found.
type VMState int

const (
	VMNotRunning    VMState = iota // no Docker backend runs, or none of its coalition's processes is the VM
	VMIdentified                   // exactly one VM process in Docker's coalition
	VMNotIdentified                // the table could not be listed, or more than one candidate matched
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
	PID          int
	StartNS      int64
	Exec         string // the executable's full path
	Coalition    uint64 // the resource coalition, read only when HasCoalition
	HasCoalition bool
}

// exeIs reports whether the last element of path is name.
func exeIs(path, name string) bool {
	return path == name || strings.HasSuffix(path, "/"+name)
}

// dockerVM lists the processes once and finds the VM among them: Docker's coalitions are those of the
// processes running com.docker.backend, and the candidates are the processes running the
// Virtualization VM in one of them.
func dockerVM(list func() ([]vmProc, bool), footprint func(pid int) (mib int64, ok bool)) VM {
	procs, ok := list()
	if !ok {
		return VM{State: VMNotIdentified}
	}
	docker := map[uint64]bool{}
	for _, p := range procs {
		if p.HasCoalition && exeIs(p.Exec, dockerBackendExe) {
			docker[p.Coalition] = true
		}
	}
	var found []vmProc
	for _, p := range procs {
		if p.HasCoalition && docker[p.Coalition] && exeIs(p.Exec, dockerVMExe) {
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
	ReadingVMState     = "DockerVMState"
	ReadingVMFootprint = "DockerVMFootprintMiB"
)

// DockerVM finds Docker Desktop's VM process: the Virtualization VM in the resource coalition of
// Docker's backend. It lists the user's processes once and keeps nothing. In a test build the
// readings hook replaces its state (0, 1 or 2) and footprint.
func DockerVM() VM {
	v := lookupVM()
	if nums, set := testhooks.Readings(); set {
		if s, ok := nums[ReadingVMState]; ok {
			v.State = VMState(s)
		}
		if f, ok := nums[ReadingVMFootprint]; ok {
			v.FootprintMiB = f
		}
	}
	return v
}
