//go:build unix

package main

import (
	"context"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/core-slot/coreslot/internal/docker"
	relay "github.com/core-slot/coreslot/internal/forward"
)

const (
	partMax      = 256             // containers one run attributes; one more makes it blind
	partDeadline = 2 * time.Second // for a first touch's call, and for all of a sample's calls
	sampleTicks  = 3               // a sample every third watch tick
)

// validRef is a reference the run puts into a request: a name or an id as the engine makes them. A
// tap line is the forwarder's reading of the command's traffic, so it is checked here, by the run.
var validRef = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")

// partContainer is one container of a run: its usage at the run's first touch, once read, and at
// the last sample, in bytes.
type partContainer struct {
	baseline, usage uint64
	based           bool
}

// dockerPart is what an engaged run's containers hold: each container it created or touched, from
// its first touch on, counts by how much it grew since then. It is blind once the run lost sight of
// them (its forwarder died or its tap lost track, a reference it cannot use, a 257th container), and
// a blind part makes no Docker call and gives no figure again. Its calls go to the upstream socket,
// never through the forwarder.
type dockerPart struct {
	sampling atomic.Bool // a sample is under way

	mu         sync.Mutex
	containers map[string]*partContainer // by id
	resolved   map[string]bool           // the names and short ids already inspected
	lost       bool
	footprint  uint64 // the last completed sample's, in bytes
	peak       uint64 // the largest footprint so far, in bytes
}

// setBlind makes the part blind.
func (p *dockerPart) setBlind() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lost = true
}

// blind says whether the part is blind.
func (p *dockerPart) blind() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lost
}

// event takes one of the tap's events. A created container's baseline is 0, with no call. A touched
// id's baseline is its usage now, one stats call; a touched name or short id is resolved by one
// inspect, the first time only, and its baseline is the first usage read after that. A container
// already counted is not touched again.
func (p *dockerPart) event(e relay.Event, sock string) {
	if p.blind() {
		return
	}
	switch e.Kind {
	case relay.KindBlind:
		p.setBlind()
	case relay.KindCreated:
		if !docker.IsID(e.Ref) {
			p.setBlind()
			return
		}
		p.add(e.Ref, func(c *partContainer) { c.based = true })
	case relay.KindTouched:
		p.touched(e.Ref, sock)
	}
}

// touched counts a touched reference's container from now on.
func (p *dockerPart) touched(ref, sock string) {
	if !validRef.MatchString(ref) {
		p.setBlind()
		return
	}
	if docker.IsID(ref) {
		if p.known(ref) {
			return
		}
		c, gone, code := docker.Stat(sock, docker.Listed{ID: ref}, time.Now().Add(partDeadline))
		p.add(ref, func(pc *partContainer) {
			if code == "" && !gone { // else the baseline is the next usage read
				pc.baseline, pc.usage, pc.based = c.UsageBytes, c.UsageBytes, true
			}
		})
		return
	}
	p.mu.Lock()
	done := p.resolved[ref]
	if p.resolved == nil {
		p.resolved = map[string]bool{}
	}
	p.resolved[ref] = true
	p.mu.Unlock()
	if done {
		return
	}
	id, code := docker.Inspect(sock, ref, time.Now().Add(partDeadline))
	if code != "" {
		p.setBlind()
		return
	}
	p.add(id, func(*partContainer) {})
}

// known says whether the part counts the container id.
func (p *dockerPart) known(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.containers[id] != nil
}

// add counts the container id, set up by first, unless the part counts it already; a 257th makes
// the part blind.
func (p *dockerPart) add(id string, first func(*partContainer)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lost || p.containers[id] != nil {
		return
	}
	if len(p.containers) == partMax {
		p.lost = true
		return
	}
	if p.containers == nil {
		p.containers = map[string]*partContainer{}
	}
	c := &partContainer{}
	first(c)
	p.containers[id] = c
}

// sample reads the usage of each of the part's containers, all under one deadline, and makes the
// result the part's footprint. It runs in a goroutine of its own, one at a time: a sample that finds
// another under way returns at once. A container whose read fails keeps its last usage; one that is
// gone uses nothing; one not read since its first touch takes this read as its baseline.
func (p *dockerPart) sample(ctx context.Context, sock string) {
	if !p.sampling.CompareAndSwap(false, true) {
		return
	}
	defer p.sampling.Store(false)
	p.mu.Lock()
	ids := make([]string, 0, len(p.containers))
	for id := range p.containers {
		ids = append(ids, id)
	}
	lost := p.lost
	p.mu.Unlock()
	if lost {
		return
	}
	type read struct {
		usage uint64
		ok    bool
	}
	reads := make(map[string]read, len(ids))
	deadline := time.Now().Add(partDeadline)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		c, gone, code := docker.Stat(sock, docker.Listed{ID: id}, deadline)
		if code == "" {
			reads[id] = read{usage: c.UsageBytes, ok: !gone}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lost {
		return
	}
	var sum uint64
	for id, c := range p.containers {
		if r, ok := reads[id]; ok {
			switch {
			case !c.based && r.ok:
				c.baseline, c.usage, c.based = r.usage, r.usage, true
			case c.based && r.ok:
				c.usage = r.usage
			case c.based:
				c.usage = 0
			}
		}
		if c.based && c.usage > c.baseline {
			sum += c.usage - c.baseline
		}
	}
	p.footprint, p.peak = sum, max(p.peak, sum)
}

// footprintMiB is the last completed sample's footprint in whole MiB, cut.
func (p *dockerPart) footprintMiB() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int64(p.footprint >> 20)
}

// peakMiB is the largest footprint so far in MiB, rounded up, so that any byte counted is at least 1.
func (p *dockerPart) peakMiB() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int64((p.peak + 1<<20 - 1) >> 20)
}
