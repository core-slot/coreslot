//go:build coreslot_testhooks

package memsample

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/core-slot/coreslot/internal/testhooks"
)

// The readings file sets DockerVM's state, footprint and backend, and ReadHost, which reads the
// same file, neither panics on those names nor takes them for host readings. Mutation: dropping the
// DockerVM prefix skip in overlay panics here; DockerVM ignoring the file keeps VMNotRunning, and
// ignoring DockerVMVMM keeps VMM false.
func TestDockerVMReadingsHook(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CORESLOT_TESTHOOK_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, testhooks.ReadingsFile), []byte("DockerVMState 1\nDockerVMFootprintMiB 4096\nDockerVMVMM 1\nDecomp 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DockerVM(); got.State != VMIdentified || got.FootprintMiB != 4096 || !got.VMM {
		t.Errorf("DockerVM = %+v, want identified on Docker VMM at 4096 MiB", got)
	}
	nums, _ := testhooks.Readings()
	if r := overlay(HostReading{}, nums); r.Decomp != 5 {
		t.Errorf("overlay = %+v, want Decomp 5", r)
	}
}
