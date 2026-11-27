//go:build unix

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/engine"
	relay "github.com/core-slot/coreslot/internal/forward"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/testhooks"
	"github.com/core-slot/coreslot/internal/wire"
)

const (
	forwarderReady = 2 * time.Second // how long a forwarder has to say ready
	eventsQueue    = 256             // events read from the pipe and not yet taken by the run
)

// engagement is docker.Engage's answer for a run: the upstream of its forwarder when engaged, else
// the note it carries ("" for a run that says nothing) and, for NoteContext, the context it names.
type engagement struct{ upstream, note, context string }

// forwarder is a run's forwarder process, alive from its ready event: addr is the address the
// command's DOCKER_HOST names, entry its directory, end the run's end of the pipe whose end ends it,
// events what its tap reports after ready (a line that does not parse is blind), exited is closed
// once it has exited and been reaped, connected is set from its first connection, and part is what
// the events attribute to the run.
type forwarder struct {
	addr, upstream, entry string
	end                   *os.File
	events                <-chan relay.Event
	exited                <-chan struct{}
	connected             bool
	part                  dockerPart
}

var errNotReady = errors.New("the forwarder did not say ready")

// startForwarder starts exe forward <entry> <upstream> in a process group of its own, with
// descriptor 3 the read end of the end pipe and 4 the write end of the events pipe, standard
// streams on the null device; the run's ends of both pipes are close-on-exec, so nothing else the
// run starts inherits them. It waits up to 2 s for the ready event; a forwarder that has not said
// it by then, or that says anything else first, has its group killed and is reaped.
func startForwarder(exe, entry, upstream string) (*forwarder, error) {
	endR, endW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	evR, evW, err := os.Pipe()
	if err != nil {
		endR.Close()
		endW.Close()
		return nil, err
	}
	cmd := exec.Command(exe, "forward", entry, upstream)
	cmd.ExtraFiles = []*os.File{endR, evW}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	endR.Close()
	evW.Close()
	if err != nil {
		endW.Close()
		evR.Close()
		return nil, err
	}
	ready := make(chan bool, 1)
	events := make(chan relay.Event, eventsQueue)
	go readEvents(evR, ready, events)
	timer := time.NewTimer(forwarderReady)
	defer timer.Stop()
	ok := false
	select {
	case ok = <-ready: // false at an end of the pipe: the forwarder exited, still unreaped
	case <-timer.C:
	}
	if !ok {
		// Not reaped yet, so its group's id is still its own.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		endW.Close()
		_ = cmd.Wait()
		return nil, errNotReady
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	addr := filepath.Join(entry, relay.SockName)
	return &forwarder{addr: addr, upstream: upstream, entry: entry, end: endW, events: events, exited: exited}, nil
}

// readEvents reads the forwarder's event lines from r: whether the first is ready goes to ready,
// and every later one to out, a line that does not parse as blind. It closes out at the pipe's end.
func readEvents(r *os.File, ready chan<- bool, out chan<- relay.Event) {
	defer r.Close()
	defer close(out)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 512), 512)
	if !sc.Scan() {
		ready <- false
		return
	}
	e, ok := relay.ParseEvent(sc.Text())
	ready <- ok && e.Kind == relay.KindReady
	if !ok || e.Kind != relay.KindReady {
		return
	}
	for sc.Scan() {
		e, ok := relay.ParseEvent(sc.Text())
		if !ok {
			e = relay.Event{Kind: relay.KindBlind}
		}
		out <- e
	}
	if sc.Err() != nil { // a line longer than any the tap writes
		out <- relay.Event{Kind: relay.KindBlind}
	}
}

// engage decides once, before the run queues, whether it gets a forwarder (docker.Engage). Off
// macOS the features do not exist and nothing is said. Under the state directory's override a
// test's hook socket stands for Docker Desktop's socket.
func (r *run) engage() {
	if runtime.GOOS != "darwin" {
		return
	}
	env := environ()
	ref := ""
	if r.loc.home != "" {
		ref = filepath.Join(r.loc.home, ".docker/run/docker.sock")
	}
	if hook := testhooks.DockerSocket(); env[config.StateDirEnv] != "" && hook != "" {
		ref = hook
	}
	selected := selectedContext(env, r.loc.home)
	upstream, note := docker.Engage(docker.EngageInput{
		Off:       r.dockerOff,
		Interest:  r.dockerInterest(),
		Switch:    r.entry.DockerForward,
		EntryBad:  r.entryBad,
		UserHost:  env["DOCKER_HOST"],
		Context:   selected,
		Reference: ref,
	})
	r.setEngagement(engagement{upstream: upstream, note: note, context: selected})
}

// dockerInterest reports whether the run had a Docker interest at its start: a container command,
// a label whose window had container work, or a docker_mem need.
func (r *run) dockerInterest() bool {
	return r.o.containerCommand() || r.marked || r.entry.DockerMemMiB > 0
}

// selectedContext is the Docker context the user selected: DOCKER_CONTEXT, else the currentContext
// of $DOCKER_CONFIG/config.json (<home>/.docker/config.json); "" when none, or the file does not read.
// A convenience against the user's own configuration: it only withholds a forwarder.
func selectedContext(env map[string]string, home string) string {
	if c := env["DOCKER_CONTEXT"]; c != "" {
		return c
	}
	dir := env["DOCKER_CONFIG"]
	if dir == "" {
		if home == "" {
			return ""
		}
		dir = filepath.Join(home, ".docker")
	}
	var c struct{ CurrentContext string }
	if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err != nil || json.Unmarshal(b, &c) != nil {
		return ""
	}
	return c.CurrentContext
}

// setEngagement records the run's engagement and says its note once, unless the user turned the
// forwarder off, by the label's docker_forward = false or [docker] enabled = false, whatever other
// reason also holds: the hold still carries the note.
func (r *run) setEngagement(fw engagement) {
	r.fw = fw
	said := fw.note // the hold's note stays a fixed word; the line names the context
	if said == docker.NoteContext {
		said += " " + text(fw.context)
	}
	userOff := r.dockerOff == docker.OffConfig || r.entry.DockerForward != nil && !*r.entry.DockerForward
	if said != "" && !userOff {
		r.notice("%s: the command's Docker work is not attributed to this run", said)
	}
}

// forwardBase is where the forwarder's directory lives: the home directory, or the state directory
// under its override.
func forwardBase(loc location) string {
	if os.Getenv(config.StateDirEnv) != "" {
		return loc.dir
	}
	return loc.home
}

// forwarderDir is the forwarder's directory as doctor reads it, never made; "" without a home.
func forwarderDir(loc location) string {
	if base := forwardBase(loc); base != "" {
		return filepath.Join(base, relay.DirName)
	}
	return ""
}

// startForwarding starts the engaged run's forwarder at its first admission, in the forwarder's
// directory of the home directory (of the state directory under its override), and then walks
// that directory for entries nothing serves. A run that cannot have one carries the reason.
func (r *run) startForwarding() {
	dir, err := relay.Dir(forwardBase(r.loc))
	if err != nil {
		r.setEngagement(engagement{note: docker.NoteAddress})
		return
	}
	name := relay.EntryName(os.Getpid(), store.NowNS())
	if _, ok := relay.Address(dir, name); !ok {
		r.setEngagement(engagement{note: docker.NoteAddress})
		return
	}
	exe, err := os.Executable()
	if err == nil {
		r.fwd, err = startForwarder(exe, filepath.Join(dir, name), r.fw.upstream)
	}
	if err != nil {
		r.setEngagement(engagement{note: docker.NoteStart})
		return
	}
	fwd := r.fwd
	go func() { // a forwarder that dies leaves its address to the upstream, its entry recreated if gone
		<-fwd.exited
		_ = relay.Swap(fwd.entry, fwd.upstream)
	}()
	go func() { _, _ = relay.Walk(dir, time.Now()) }()
}

// endForwarder ends the run's forwarder, if it has one, without waiting for it: it swaps its
// address and serves its open connections to their end.
func (r *run) endForwarder() {
	if r.fwd != nil {
		r.fwd.end.Close()
	}
}

// forwarderEvents takes the events the forwarder reported since the last listing: a first
// connection makes the run container work, the others go to its Docker part, and the forwarder's
// death makes the part blind. The run says once that it went blind.
func (r *run) forwarderEvents(p *peak) {
	part := &r.fwd.part
	if !part.blind() {
		defer func() {
			if part.blind() {
				r.notice("%s: the command's Docker work is no longer attributed to this run", docker.NoteBlind)
			}
		}()
	}
	for {
		select {
		case e, ok := <-r.fwd.events:
			if !ok {
				part.setBlind()
				return
			}
			if e.Kind == relay.KindConnected {
				r.fwd.connected, p.container = true, true
			} else {
				part.event(e)
			}
			continue
		case <-r.fwd.exited:
			part.setBlind()
		default:
		}
		return
	}
}

// dockerNote is the note the run's hold carries: ungated: blind once the run lost sight of its
// containers, else why the engine ungated the attempt's Docker part, else its engagement's note.
func (r *run) dockerNote() string {
	switch {
	case r.fwd != nil && r.fwd.part.blind():
		return docker.NoteBlind
	case r.ungated != "":
		return r.ungated
	}
	return r.fw.note
}

// dockerGated reports whether the attempt's hold still reserves its Docker need's growth: it was
// admitted on it, and its run sees its containers.
func (r *run) dockerGated() bool {
	return r.gate != nil && r.fwd != nil && !r.fwd.part.blind()
}

// growing says whether the attempt's hold may still reserve growth its containers have taken: it
// is gated and their footprint is below its need. Its run samples them every tick until then, so
// that the reservation drops one tick after they grew.
func (r *run) growing() bool {
	return r.dockerGated() && r.fwd.part.footprintMiB() < r.gate.needMiB
}

// setDockerKeys writes into h, for a grant admitted on its Docker need, the growth and stamp of
// the attempt's last sample, and drops its Docker keys once the attempt is no longer gated (its
// forwarder did not start, or the run lost sight of its containers). Before the first sample the
// admission's keys stay.
func (r *run) setDockerKeys(g *engine.Grant, h *wire.Hold) {
	switch {
	case !g.DockerGated():
	case !r.dockerGated():
		h.DockerNeedMiB, h.DockerGrowthMiB, h.DockerSampledNS = 0, 0, 0
	default:
		growth, known, ns := r.gate.last()
		if known {
			h.DockerGrowthMiB = growth
		}
		if ns > 0 {
			h.DockerSampledNS = ns
		}
	}
}

// sampleDocker samples the containers of fwd's part and, for an attempt gated by gate (nil for
// one that is not), the VM growth its hold reserves; it runs off the tick path.
func sampleDocker(ctx context.Context, fwd *forwarder, gate *dockerGate) {
	part := &fwd.part
	if part.sample(ctx, fwd.upstream) && gate != nil {
		gate.sampled(part.footprintMiB(), store.NowNS())
	}
}

// dockerFootprint is the footprint the run's hold carries: its Docker part's once its forwarder
// relayed a connection and while it is not blind, 0 included, else none. Before that connection a
// run that is container work, by a client that bypasses the forwarder, has its containers' memory
// unmeasured.
func (r *run) dockerFootprint() *int64 {
	if r.fwd == nil || !r.fwd.connected || r.fwd.part.blind() {
		return nil
	}
	f := r.fwd.part.footprintMiB()
	return &f
}

// withDockerHost is env with DOCKER_HOST naming addr, whatever it named before.
func withDockerHost(env []string, addr string) []string {
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, "DOCKER_HOST=") })
	return append(env, "DOCKER_HOST=unix://"+addr)
}
