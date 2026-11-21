//go:build unix

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
	"github.com/core-slot/coreslot/internal/engine"
	relay "github.com/core-slot/coreslot/internal/forward"
	"github.com/core-slot/coreslot/internal/spec"
)

const partMiB = 1 << 20

// partEngine is the HTTP fake with one running container, id, named name, using usage bytes.
func partEngine(t *testing.T, id, name string, usage uint64, opts ...dockertest.Option) *dockertest.Fake {
	t.Helper()
	c := dockertest.Container{ID: id, Name: name, UsageBytes: usage, LimitBytes: 8 << 30}
	return dockertest.HTTP(t, append([]dockertest.Option{dockertest.WithContainer(c)}, opts...)...)
}

// statsCalls are the stats requests f received.
func statsCalls(f *dockertest.Fake) (out []string) {
	for _, r := range f.Requests() {
		if strings.Contains(r, "/stats") {
			out = append(out, r)
		}
	}
	return out
}

func created(id string) relay.Event   { return relay.Event{Kind: relay.KindCreated, Ref: id} }
func touchedRef(r string) relay.Event { return relay.Event{Kind: relay.KindTouched, Ref: r} }

// touchNow is a touch of ref and its first-touch read, as the watchdog's loop starts it.
func touchNow(p *dockerPart, ref, sock string) {
	p.event(touchedRef(ref))
	p.readTouches(sock)
}

// A container the run created counts from 0, and its creation makes no call. Mutation: a created
// container's baseline left to its first read gives 0, red.
func TestCreatedBaselineZero(t *testing.T) {
	id := dockertest.ID('a')
	f := partEngine(t, id, "db", 300*partMiB)
	var p dockerPart
	p.event(created(id))
	if r := f.Requests(); len(r) != 0 {
		t.Errorf("a creation made calls %q", r)
	}
	p.sample(context.Background(), f.Sock)
	if got := p.footprintMiB(); got != 300 {
		t.Errorf("footprint %d MiB, want 300", got)
	}
}

// A container touched by its id counts from its usage at the touch, read by exactly one call then.
// Mutations: the baseline left to the next sample gives 0, red; an inspect before the stats call
// makes two calls at the touch, red.
func TestTouchedIDBaselineAtFirstTouch(t *testing.T) {
	id := dockertest.ID('b')
	f := partEngine(t, id, "db", 500*partMiB)
	var p dockerPart
	touchNow(&p, id, f.Sock)
	if r := f.Requests(); len(r) != 1 || len(statsCalls(f)) != 1 {
		t.Errorf("calls at the touch %q, want one stats call", r)
	}
	touchNow(&p, id, f.Sock) // touched again: counted already
	f.SetUsage(id, 800*partMiB)
	p.sample(context.Background(), f.Sock)
	if got := p.footprintMiB(); got != 300 {
		t.Errorf("footprint %d MiB, want 300", got)
	}
	if n := len(f.Requests()); n != 2 {
		t.Errorf("%d calls %q, want the touch's and the sample's", n, f.Requests())
	}
}

// A name is resolved by one inspect at its first touch and never again; the container's baseline is
// the next sample's read. Mutation: forget that the name was resolved, and its second touch
// inspects again, red.
func TestTouchedNameResolvedOnce(t *testing.T) {
	id := dockertest.ID('c')
	f := partEngine(t, id, "db", 100*partMiB)
	var p dockerPart
	touchNow(&p, "db", f.Sock)
	if r := f.Requests(); len(r) != 1 || r[0] != "GET /containers/db/json" {
		t.Errorf("calls at the touch %q, want one inspect", r)
	}
	touchNow(&p, "db", f.Sock)
	p.sample(context.Background(), f.Sock)
	if got := p.footprintMiB(); got != 0 {
		t.Errorf("footprint %d MiB at the first sample, want 0: it is the baseline", got)
	}
	f.SetUsage(id, 250*partMiB)
	p.sample(context.Background(), f.Sock)
	if got := p.footprintMiB(); got != 150 {
		t.Errorf("footprint %d MiB, want 150", got)
	}
	path, _ := docker.StatsPath(id)
	want := []string{"GET /containers/db/json", "GET " + path, "GET " + path}
	if r := f.Requests(); strings.Join(r, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls %q, want %q", r, want)
	}
}

// A first touch whose read fails takes its baseline from the next read, never 0. Mutation: a failed
// read taken as a baseline of 0 gives 400 at the first sample, red.
func TestFirstTouchReadFailsBaselineIsNextRead(t *testing.T) {
	id := dockertest.ID('d')
	var calls atomic.Int64
	f := dockertest.Serve(t, func(c net.Conn, _ string) {
		switch calls.Add(1) {
		case 1:
			dockertest.Reply(c, 500, `{"message":"busy"}`)
		case 2:
			dockertest.Reply(c, 200, dockertest.Stats(400*partMiB, 8<<30, ""))
		default:
			dockertest.Reply(c, 200, dockertest.Stats(600*partMiB, 8<<30, ""))
		}
	})
	var p dockerPart
	touchNow(&p, id, f.Sock)
	p.sample(context.Background(), f.Sock)
	if got := p.footprintMiB(); got != 0 {
		t.Errorf("footprint %d MiB at the first read, want 0", got)
	}
	p.sample(context.Background(), f.Sock)
	if got := p.footprintMiB(); got != 200 {
		t.Errorf("footprint %d MiB, want 200", got)
	}
}

// A reference the run would not put into a request makes it blind, with no call; the longest valid
// name is used. Mutation: drop the regex's anchor ^, and -x, ../x and the 129-byte name match their
// tails, red.
func TestRefValidation(t *testing.T) {
	long := strings.Repeat("n", 128)
	f := partEngine(t, dockertest.ID('e'), long, partMiB)
	for _, c := range []struct {
		ref   string
		blind bool
	}{{"../x", true}, {"n" + long, true}, {"-x", true}, {"", true}, {long, false}} {
		before := len(f.Requests())
		var p dockerPart
		touchNow(&p, c.ref, f.Sock)
		calls := len(f.Requests()) - before
		if p.blind() != c.blind || c.blind && calls != 0 {
			t.Errorf("ref %q: blind %v after %d calls, want blind %v, with no call when blind", c.ref, p.blind(), calls, c.blind)
		}
	}
}

// A reference the engine does not resolve makes the run blind, and a blind part calls no more.
// Mutation: ignore the inspect's failure, and the part stays sighted, red.
func TestUnresolvedRefIsBlind(t *testing.T) {
	f := partEngine(t, dockertest.ID('f'), "db", partMiB)
	var p dockerPart
	p.event(created(dockertest.ID('f')))
	touchNow(&p, "nosuch", f.Sock)
	p.sample(context.Background(), f.Sock)
	if !p.blind() || len(f.Requests()) != 1 {
		t.Errorf("blind %v after calls %q, want blind after the inspect alone", p.blind(), f.Requests())
	}
}

// A run counts at most 256 containers: a 257th makes it blind, a container counted already does not.
// Mutations: the bound tested with > lets a 257th in, red; the bound tested before the container
// counted already makes the repeat blind, red.
func TestTwoHundredFiftySeventhIsBlind(t *testing.T) {
	var p dockerPart
	for i := range partMax {
		p.event(created(fmt.Sprintf("%064x", i)))
	}
	p.event(created(fmt.Sprintf("%064x", 0)))
	if p.blind() {
		t.Fatal("blind at 256 containers")
	}
	p.event(created(fmt.Sprintf("%064x", partMax)))
	if !p.blind() {
		t.Error("a 257th container left the part sighted")
	}
}

// The tap's blind event makes the part blind, and it counts nothing after. Mutation: drop the
// blind case, and the part stays sighted, red.
func TestBlindEventIsBlind(t *testing.T) {
	f := partEngine(t, dockertest.ID('a'), "db", partMiB)
	var p dockerPart
	p.event(relay.Event{Kind: relay.KindBlind})
	touchNow(&p, dockertest.ID('a'), f.Sock)
	p.sample(context.Background(), f.Sock)
	if !p.blind() || len(f.Requests()) != 0 {
		t.Errorf("blind %v after calls %q, want blind with none", p.blind(), f.Requests())
	}
}

// engagedRun is a run whose forwarder has relayed a connection and reports what the test sends on
// its channel, with the fake engine f its upstream and its notices in a file of its own.
func engagedRun(t *testing.T, f *dockertest.Fake) (r *run, events chan relay.Event, stderr string) {
	t.Helper()
	stderr = filepath.Join(t.TempDir(), "stderr")
	file, err := os.Create(stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	events = make(chan relay.Event, eventsQueue)
	r = restartable("make", "test")
	r.io.Stderr = file
	r.fwd = &forwarder{upstream: f.Sock, events: events, exited: make(chan struct{}), connected: true}
	return r, events, stderr
}

// An engaged run whose forwarder relayed a connection carries its Docker footprint from its first
// hold write, 0 included; one whose forwarder has relayed none, and a run that is not engaged, carry
// none. Mutations: write the footprint only above 0 → the connected row, red; write it before a
// connection → the unconnected row, red.
func TestEngagedHoldCarriesZeroFootprint(t *testing.T) {
	f := dockertest.HTTP(t)
	for _, c := range []struct {
		name               string
		engaged, connected bool
	}{{"connected", true, true}, {"no connection yet", true, false}, {"not engaged", false, false}} {
		g, st := watchedHold(t)
		r, _, _ := engagedRun(t, f)
		if c.engaged {
			r.fwd.connected = c.connected
		} else {
			r.fwd = nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r.watchChild(ctx, &supervisor{pid: 42, group: true}, g, st)
		if got := body(t, st, g).DockerFootprintMiB; c.connected && (got == nil || *got != 0) || !c.connected && got != nil {
			t.Errorf("%s: docker_footprint_mib %v", c.name, got)
		}
	}
}

// An engaged run reads its containers at every third tick only, each by its id. Listing n is read
// before its tick, so the sample of tick k (listing 2k) shows from listing 2k+1. Mutation: a sample
// at every tick shows at listing 3, red.
func TestSamplesEveryThirdTick(t *testing.T) {
	id := dockertest.ID('a')
	f := partEngine(t, id, "db", partMiB)
	g, st := watchedHold(t)
	r, events, _ := engagedRun(t, f)
	events <- created(id)
	path, _ := docker.StatsPath(id)
	watchFor(t, r, &supervisor{pid: 42, group: true}, g, st, 19, func(int) bool { return false }, func(n int) {
		if got, want := len(statsCalls(f)), (n-1)/(listingsPerTick*sampleTicks); got != want {
			t.Errorf("listing %d: %d stats calls, want %d", n, got, want)
		}
	})
	for _, c := range statsCalls(f) {
		if c != "GET "+path {
			t.Errorf("stats call %q, want by the id", c)
		}
	}
}

// Once blind, the run says so once, its hold carries ungated: blind and no footprint, and it reads
// no more. Mutations: a sample that ignores blindness reads at tick 6, red; the notice said at
// every listing, red; the footprint written while blind, red.
func TestBlindWritesNoteAndStops(t *testing.T) {
	id := dockertest.ID('a')
	f := partEngine(t, id, "db", partMiB)
	g, st := watchedHold(t)
	r, events, stderr := engagedRun(t, f)
	events <- created(id)
	watchFor(t, r, &supervisor{pid: 42, group: true}, g, st, 13, func(int) bool { return false }, func(n int) {
		if n == 7 {
			events <- relay.Event{Kind: relay.KindBlind}
		}
	})
	time.Sleep(100 * time.Millisecond) // a sample started at the last tick ends
	if n := len(statsCalls(f)); n != 1 {
		t.Errorf("%d stats calls, want tick 3's alone", n)
	}
	h := body(t, st, g)
	if h.DockerNote != docker.NoteBlind || h.DockerFootprintMiB != nil {
		t.Errorf("hold docker_note %q, docker_footprint_mib %v; want %q and none", h.DockerNote, h.DockerFootprintMiB, docker.NoteBlind)
	}
	said, _ := os.ReadFile(stderr)
	if n := strings.Count(string(said), docker.NoteBlind); n != 1 {
		t.Errorf("stderr %q says %q %d times, want once", said, docker.NoteBlind, n)
	}
}

// A hung engine never stalls the watch: while touches' first reads and samples hang, the listings
// keep their 1 s period, every tick writes the hold, and a reader finds the one under way and
// returns. Mutations: the first-touch reads on the watchdog's loop stall listing 2 for 2 s, red;
// the sample on the tick goroutine stalls a listing likewise, red; drop the one-at-a-time guard, and
// the second sample waits and calls too, red.
func TestHungEngineNeverStallsTick(t *testing.T) {
	id := dockertest.ID('a')
	f := partEngine(t, id, "db", partMiB, dockertest.Hang())
	t.Run("ticks", func(t *testing.T) {
		g, st := watchedHold(t)
		r, events, _ := engagedRun(t, f)
		events <- created(id)
		var last time.Time
		watchFor(t, r, &supervisor{pid: 42, group: true}, g, st, 14, func(int) bool { return false }, func(n int) {
			now := time.Now()
			if gap := now.Sub(last); n > 1 && gap > 1500*time.Millisecond {
				t.Errorf("listing %d came %v after the one before", n, gap)
			}
			last = now
			if n >= 2 && n <= 4 { // touches whose first-touch reads hang
				events <- touchedRef(dockertest.ID(byte('a' + n - 1)))
			}
			if h := body(t, st, g); n > 2 && n%2 == 1 && (h.FootprintMiB != int64(n-1) || h.DockerFootprintMiB == nil) {
				t.Errorf("listing %d: hold footprint_mib %d, docker_footprint_mib %v; want tick %d's write", n, h.FootprintMiB, h.DockerFootprintMiB, (n-1)/2)
			}
		})
	})
	t.Run("one at a time", func(t *testing.T) {
		before := len(statsCalls(f))
		var p dockerPart
		p.event(created(id))
		go p.sample(context.Background(), f.Sock)
		waitFor(t, 2*time.Second, "the first sample's call", func() bool { return len(statsCalls(f)) == before+1 })
		start := time.Now()
		p.sample(context.Background(), f.Sock)
		if took := time.Since(start); took > 500*time.Millisecond || len(statsCalls(f)) != before+1 {
			t.Errorf("a second sample took %v and made %d calls, want at once and none", took, len(statsCalls(f))-before-1)
		}
	})
}

// The selected context is DOCKER_CONTEXT, else config.json's currentContext under DOCKER_CONFIG,
// else under the home directory's .docker; a missing or unparsable file selects none. Mutation:
// ignore DOCKER_CONFIG → the "config dir" row reads the home's file, red.
func TestSelectedContext(t *testing.T) {
	home, cfg := t.TempDir(), t.TempDir()
	for dir, body := range map[string]string{filepath.Join(home, ".docker"): `{"currentContext":"home-ctx"}`, cfg: `{"auths":{},"currentContext":"cfg-ctx"}`} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "config.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		env  map[string]string
		home string
		want string
	}{
		{"variable", map[string]string{"DOCKER_CONTEXT": "env-ctx", "DOCKER_CONFIG": cfg}, home, "env-ctx"},
		{"config dir", map[string]string{"DOCKER_CONFIG": cfg}, home, "cfg-ctx"},
		{"home", map[string]string{}, home, "home-ctx"},
		{"missing", map[string]string{"DOCKER_CONFIG": t.TempDir()}, home, ""},
		{"unparsable", map[string]string{"DOCKER_CONFIG": bad}, home, ""},
		{"no home", map[string]string{}, "", ""},
	} {
		if got := selectedContext(c.env, c.home); got != c.want {
			t.Errorf("%s: selectedContext = %q, want %q", c.name, got, c.want)
		}
	}
}

// A gated command coming back after a stop keeps its Docker need, and its request carries what its
// containers use now, from which its first body reserves the growth; a run that lost sight of them
// comes back without either. Mutation: requeue leaving DockerFootprintMiB at 0 reddens the first row.
func TestRestartKeepsDockerNeedAndFootprint(t *testing.T) {
	for _, blind := range []bool{false, true} {
		r := &run{dockerNeedMiB: 2048, fwd: &forwarder{connected: true}}
		r.fwd.part.footprint = 700*partMiB + 5
		if blind {
			r.fwd.part.setBlind()
		}
		req := spec.Request{NeedMiB: 64, DockerNeedMiB: 2048}
		r.requeue(&req, &engine.Grant{})
		want := [2]int64{2048, 700}
		if blind {
			want = [2]int64{}
		}
		if got := [2]int64{req.DockerNeedMiB, req.DockerFootprintMiB}; got != want || !req.Restart {
			t.Errorf("blind %v: Docker need and footprint %v, restart %v; want %v and a restart", blind, got, req.Restart, want)
		}
	}
}
