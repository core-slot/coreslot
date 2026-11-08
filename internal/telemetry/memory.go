//go:build unix

package telemetry

import (
	"time"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/store"
)

// Sweep is the process sweep the memory line reads; a test replaces it.
var Sweep = memsample.Sweep

// Cadence is how often a run asks for the machine-wide lines.
const Cadence = 30 * time.Second

// lineEvery is how long a memory or containers line holds its stamp: under Cadence, so that a lone
// run whose ask comes a moment early does not miss a window and write every other time.
const lineEvery = Cadence - 5*time.Second

// topN is how many of the largest processes a memory line names.
const topN = 5

// MemoryAsync writes a memory line when none was written within lineEvery by any process: it
// claims memory.stamp, and the claimant sweeps and writes in a goroutine, so the caller never waits
// for the sweep. Two processes may both claim in one window and write two lines. The line names
// no run and is written only where Sweep reads, which is macOS; it never reads the Docker socket.
func (r *Recorder) MemoryAsync() {
	if r == nil || r.Stopped() || !r.claim(store.MemoryStamp) {
		return
	}
	r.memWG.Add(1)
	go func() {
		defer r.memWG.Done()
		defer r.recoverLost()
		top, vm, ok := Sweep(topN)
		if !ok {
			return
		}
		f := Fields{"top": topLines(top)}
		if vm != nil {
			f["vm"] = Fields{"name": vm.Name, "footprint_mib": vm.FootprintMiB, "resident_mib": vm.ResidentMiB, "cpu_s": vm.CPUS, "start_ns": vm.StartNS}
		}
		r.EmitMachine("memory", f)
	}()
}

// WaitAsync waits for the goroutines MemoryAsync and ContainersAsync started. It exists for tests;
// production never calls it.
func (r *Recorder) WaitAsync() { r.memWG.Wait() }

func topLines(top []memsample.ProcMem) []Fields {
	out := make([]Fields, 0, len(top))
	for _, p := range top {
		out = append(out, Fields{"name": p.Name, "footprint_mib": p.FootprintMiB})
	}
	return out
}

// claim reports whether the named stamp is due and touches it through its descriptor, with no
// lock: the race of two claimants is accepted. It is due when never claimed (empty), lineEvery old or
// in the future (the clock went back). A stamp it cannot open or touch is not claimed.
func (r *Recorder) claim(stamp string) (won bool) {
	defer r.recoverLost()
	f, err := store.OpenStamp(r.dir, stamp)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	age := time.Since(fi.ModTime())
	if fi.Size() > 0 && age >= 0 && age < lineEvery {
		return false
	}
	_, err = f.WriteAt([]byte{'\n'}, 0) // a write through the descriptor is the touch
	return err == nil
}

// recoverLost recovers a panic of the recorder's own goroutine and counts it.
func (r *Recorder) recoverLost() {
	if recover() != nil {
		r.mu.Lock()
		r.lost++
		r.mu.Unlock()
	}
}
