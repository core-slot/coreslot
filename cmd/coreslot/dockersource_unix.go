//go:build unix

package main

import (
	"context"
	"sync"
	"time"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
)

// dockerReadDeadline bounds one reading of a gated run: every call of it together.
const dockerReadDeadline = 2 * time.Second

// newDockerSource is a gated run's reading of Docker's VM, from the first of sockets that answers.
// The VM's process is looked for first, so that no socket is opened while Docker's VM does not
// run. A reading's instant is taken just before its first request, on the engine's clock. A VM not
// identified, an engine that cannot be read, or another engine is no reading.
func newDockerSource(sockets []string) engine.DockerSource {
	return engine.DockerSource{Read: func(context.Context) engine.DockerReading {
		vm := memsample.DockerVM()
		started := store.NowNS()
		if vm.State != memsample.VMIdentified {
			return engine.DockerReading{StartedNS: started}
		}
		r := docker.Read(sockets, time.Now().Add(dockerReadDeadline))
		if r.Code != "" || r.OtherEngine {
			return engine.DockerReading{StartedNS: started}
		}
		return engine.DockerReading{OK: true, VMMiB: r.VMMiB, FreeEstMiB: r.FreeEstMiB(), VMNowMiB: vm.FootprintMiB, StartedNS: started}
	}}
}

// dockerGate is an attempt admitted on its Docker need: the VM growth its hold reserves on the
// Mac, from V of the reading that admitted it, its need, and at each sample of its containers
// their footprint and what the VM holds now. The VM is read from the process last identified,
// one proc_pid_rusage and no Docker call, and looked for again when that process is gone.
type dockerGate struct {
	needMiB, vMiB int64

	mu        sync.Mutex
	vm        memsample.VM // the VM process last identified
	growthMiB int64        // the growth of the last sample that found the VM
	known     bool         // a sample found the VM: growthMiB is its
	sampledNS int64        // when the last sample read the containers; 0 before the first
}

// sampled takes a sample of the attempt's containers that ended at sampledNS with footprintMiB.
// With no VM identified the growth stays the last one: unknown is not zero.
func (d *dockerGate) sampled(footprintMiB, sampledNS int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sampledNS = sampledNS
	now, ok := int64(0), false
	if d.vm.State == memsample.VMIdentified {
		now, ok = memsample.VMFootprint(d.vm.PID, d.vm.StartNS)
	}
	if !ok {
		if d.vm = memsample.DockerVM(); d.vm.State == memsample.VMIdentified {
			now, ok = d.vm.FootprintMiB, true
		}
	}
	if ok {
		d.growthMiB, d.known = min(state.Growth(d.vMiB, now, d.needMiB, footprintMiB), spec.MaxNeedMiB), true
	}
}

// last is the growth of the last sample that found the VM (known false before one did) and when
// the last sample read the containers.
func (d *dockerGate) last() (growthMiB int64, known bool, sampledNS int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.growthMiB, d.known, d.sampledNS
}
