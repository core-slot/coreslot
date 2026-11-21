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
	reading atomic.Bool // a reader is under way: readTouches or sample

	mu         sync.Mutex
	containers map[string]*partContainer // by id
	seen       map[string]bool           // the references touched so far
	touches    []string                  // the ones readTouches has not read yet, in order
	lost       bool
	footprint  uint64 // the last completed sample's, in bytes
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

// event takes one of the tap's events, with no Docker call: the watchdog's loop takes them. A
// created container's baseline is 0. A reference touched for the first time waits for readTouches.
func (p *dockerPart) event(e relay.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.lost:
	case e.Kind == relay.KindBlind:
		p.lost = true
	case e.Kind == relay.KindCreated && !docker.IsID(e.Ref), e.Kind == relay.KindTouched && !validRef.MatchString(e.Ref):
		p.lost = true
	case e.Kind == relay.KindCreated:
		p.addLocked(e.Ref, partContainer{based: true})
	case e.Kind == relay.KindTouched && !p.seen[e.Ref]:
		if p.seen == nil {
			p.seen = map[string]bool{}
		}
		p.seen[e.Ref] = true
		p.touches = append(p.touches, e.Ref)
	}
}

// touching says whether references wait for readTouches.
func (p *dockerPart) touching() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.lost && len(p.touches) > 0
}

// readTouches makes the first-touch call of each reference waiting, unless its container is
// counted already: a 64-hex id's stats, whose usage is its baseline (after a failed read, the next
// read is), or a name's or short id's inspect, whose baseline is the next sample's read. A
// reference the engine does not resolve makes the part blind. It runs in a goroutine of its own,
// and one reader at a time, this or a sample: one that finds another under way returns at once.
func (p *dockerPart) readTouches(sock string) {
	if !p.reading.CompareAndSwap(false, true) {
		return
	}
	defer p.reading.Store(false)
	for {
		p.mu.Lock()
		if p.lost || len(p.touches) == 0 {
			p.mu.Unlock()
			return
		}
		id := p.touches[0]
		p.touches = p.touches[1:]
		known := p.containers[id] != nil
		p.mu.Unlock()
		var c partContainer
		switch code := ""; {
		case known:
			continue
		case docker.IsID(id):
			s, gone, code := docker.Stat(sock, docker.Listed{ID: id}, time.Now().Add(partDeadline))
			if code == "" && !gone {
				c = partContainer{baseline: s.UsageBytes, usage: s.UsageBytes, based: true}
			}
		default:
			if id, code = docker.Inspect(sock, id, time.Now().Add(partDeadline)); code != "" {
				p.setBlind()
				return
			}
		}
		p.mu.Lock()
		if !p.lost {
			p.addLocked(id, c)
		}
		p.mu.Unlock()
	}
}

// addLocked counts the container id from c, unless the part counts it already; a 257th makes the
// part blind. The caller holds the lock.
func (p *dockerPart) addLocked(id string, c partContainer) {
	if p.containers[id] != nil {
		return
	}
	if len(p.containers) == partMax {
		p.lost = true
		return
	}
	if p.containers == nil {
		p.containers = map[string]*partContainer{}
	}
	p.containers[id] = &c
}

// sample reads the usage of each of the part's containers, all under one deadline, and makes the
// result the part's footprint. It runs in a goroutine of its own, one reader at a time, as
// readTouches does. A container whose read fails keeps its last usage; one that is
// gone uses nothing; one not read since its first touch takes this read as its baseline.
func (p *dockerPart) sample(ctx context.Context, sock string) {
	if !p.reading.CompareAndSwap(false, true) {
		return
	}
	defer p.reading.Store(false)
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
	p.footprint = sum
}

// footprintMiB is the last completed sample's footprint in whole MiB, cut.
func (p *dockerPart) footprintMiB() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int64(p.footprint >> 20)
}
