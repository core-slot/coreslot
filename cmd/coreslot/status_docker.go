//go:build unix

package main

import (
	"os"
	"runtime"
	"time"
	"unicode/utf8"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
)

// dockerReadBudget is the whole `status --docker` read: one deadline for every call.
const dockerReadBudget = 2 * time.Second

// The states of the docker object.
const (
	dockerOK          = "ok"
	dockerOff         = "off"
	dockerNotRunning  = "not_running"
	dockerOtherEngine = "other_engine"
	dockerNoReading   = "no_reading"
)

// dockerView is what `status --docker` read: a state, why (the Off value, or the reading's code), and
// in the ok state the VM and the running containers. Every other state carries none of them.
type dockerView struct {
	state, reason string
	vm            memsample.VM
	vmMiB         int64
	freeEst       int64
	containers    []docker.Container // each string already cut
}

// readDocker takes the one reading of this call, in the order that opens no socket for nothing: the
// features, then the VM process, then the engine. Docker off for any reason but no-socket is off;
// no socket, or no VM process, is not running.
func readDocker(enabled bool, loc location) *dockerView {
	env := environ()
	_, hold, miss := engine.DecideNested(os.Environ(), isAncestor)
	sockets, off := docker.Features(runtime.GOOS, enabled, env, loc.home, inherited(env, hold, miss))
	if off != docker.On && off != docker.OffNoSocket {
		return &dockerView{state: dockerOff, reason: string(off)}
	}
	vm := memsample.DockerVM()
	if off == docker.OffNoSocket || vm.State == memsample.VMNotRunning {
		return &dockerView{state: dockerNotRunning}
	}
	r := docker.Read(sockets, time.Now().Add(dockerReadBudget))
	switch {
	case !r.OK:
		return &dockerView{state: dockerNoReading, reason: r.Code}
	case r.OtherEngine:
		return &dockerView{state: dockerOtherEngine}
	}
	v := &dockerView{state: dockerOK, vm: vm, vmMiB: r.VMMiB}
	for _, c := range r.Containers {
		c.Name, c.Image, c.Project = cutBytes(c.Name, spec.MaxLabelBytes), cutBytes(c.Image, spec.MaxLabelBytes), cutBytes(c.Project, spec.MaxLabelBytes)
		v.containers = append(v.containers, c)
	}
	v.freeEst = r.FreeEstMiB()
	return v
}

// cutBytes is s cut to at most n bytes, on a rune boundary: a label's own limit, so that an engine's
// strings are shown as labels are.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// human prints the block, one line per container.
func (d *dockerView) human(p func(format string, args ...any)) {
	switch d.state {
	case dockerOff:
		p("docker: off (%s)", d.reason)
	case dockerNotRunning:
		p("docker: not running (no Docker Desktop VM was found)")
	case dockerOtherEngine:
		p("docker: the engine is not Docker Desktop; no figures")
	case dockerNoReading:
		p("docker: no reading (%s)", d.reason)
	default:
		foot := "footprint not known"
		if d.vm.State == memsample.VMIdentified {
			foot = "footprint " + spec.FormatMiB(d.vm.FootprintMiB)
		}
		p("docker: V %s  %s  free_est %s (an estimate: V less the containers' usage)", spec.FormatMiB(d.vmMiB), foot, spec.FormatMiB(d.freeEst))
		if len(d.containers) == 0 {
			p("containers: none running")
		} else {
			p("containers:")
		}
		for _, c := range d.containers {
			project := "(no project)"
			if c.Project != "" {
				project = text(c.Project)
			}
			p("  %s  %s  %s  project %s  usage %s of %s", text(docker.ShortID(c.ID)), text(c.Name), text(c.Image), project,
				spec.FormatMiB(c.UsageMiB), spec.FormatMiB(c.LimitMiB))
		}
	}
}

// jsonDocker is the docker object, present only with --docker. Every key is always there: vm, its
// nulls and free_est_mib are empty in every state but ok, and containers is [] then.
type jsonDocker struct {
	State      string          `json:"state"` // ok, off, not_running, other_engine or no_reading
	Reason     string          `json:"reason"`
	VM         jsonDockerVM    `json:"vm"`
	FreeEstMiB *int64          `json:"free_est_mib"`
	Containers []jsonContainer `json:"containers"`
}

type jsonDockerVM struct {
	MemMiB       *int64 `json:"mem_mib"`
	Identified   bool   `json:"identified"`
	FootprintMiB *int64 `json:"footprint_mib"` // null unless identified
}

type jsonContainer struct {
	ID       string `json:"id"` // the full id
	Name     string `json:"name"`
	Image    string `json:"image"`
	Project  string `json:"project"`
	UsageMiB int64  `json:"usage_mib"`
	LimitMiB int64  `json:"limit_mib"`
}

func (d *dockerView) document() *jsonDocker {
	j := &jsonDocker{State: d.state, Reason: d.reason, Containers: []jsonContainer{}}
	if d.state != dockerOK {
		return j
	}
	j.VM.MemMiB, j.FreeEstMiB = &d.vmMiB, &d.freeEst
	if d.vm.State == memsample.VMIdentified {
		j.VM.Identified, j.VM.FootprintMiB = true, &d.vm.FootprintMiB
	}
	for _, c := range d.containers {
		j.Containers = append(j.Containers, jsonContainer{ID: c.ID, Name: c.Name, Image: c.Image, Project: c.Project,
			UsageMiB: c.UsageMiB, LimitMiB: c.LimitMiB})
	}
	return j
}
