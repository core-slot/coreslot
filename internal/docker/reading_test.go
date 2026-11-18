//go:build unix

package docker_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
)

const mib = 1 << 20

func container(id string) dockertest.Container {
	return dockertest.Container{ID: id, Name: id, Image: "x", UsageBytes: 10 * mib, LimitBytes: 100 * mib}
}

// Both containers with the name's slash cut, the compose project, usage less inactive file and the
// limit, and the VM size in MiB. Mutations: a name kept with its slash, usage taken whole, or
// MemTotal read in bytes each redden the comparison.
func TestBudgetReadingComplete(t *testing.T) {
	f := dockertest.Engine(t,
		dockertest.WithInfo(docker.DesktopOS, 27_268_218_880),
		dockertest.WithContainer(dockertest.Container{ID: dockertest.LongID, Name: "db", Image: "postgres:16", Project: "shop",
			UsageBytes: 400 * mib, InactiveFileBytes: 100 * mib, LimitBytes: 2048 * mib}),
		dockertest.WithContainer(dockertest.Container{ID: dockertest.ID('c'), Name: "cache", Image: "alpine", UsageBytes: 5 * mib, LimitBytes: 1024 * mib}))
	r := docker.Read([]string{f.Sock}, dockertest.Soon())
	want := []docker.Container{
		{ID: dockertest.LongID, Name: "db", Image: "postgres:16", Project: "shop", UsageMiB: 300, LimitMiB: 2048},
		{ID: dockertest.ID('c'), Name: "cache", Image: "alpine", UsageMiB: 5, LimitMiB: 1024}}
	if r.Code != "" || r.OtherEngine || r.VMMiB != 26005 || !reflect.DeepEqual(r.Containers, want) {
		t.Errorf("reading %+v, want OK, VM 26005 and %+v", r, want)
	}
}

// A hung engine is no reading, and within the one deadline. Mutation: a read without a deadline
// outlasts 2.5 s (the test times out).
func TestBudgetReadingHungEngineIsNothing(t *testing.T) {
	f := dockertest.Engine(t, dockertest.Hang())
	start := time.Now()
	r := docker.Read([]string{f.Sock}, dockertest.Soon())
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Errorf("took %v", took)
	}
	if r.Code != "deadline" || r.Containers != nil {
		t.Errorf("reading %+v, want none with deadline", r)
	}
}

// Every call is within the deadline and all of them are not: answers 0.8 s apart put the third call
// past 2 s. Mutation: a deadline of 2 s taken afresh for each call gives OK, after 2.4 s.
func TestBudgetReadingSlowEngineIsOneDeadline(t *testing.T) {
	f := dockertest.Engine(t, dockertest.Slow(800*time.Millisecond), dockertest.WithContainer(container(dockertest.ID('a'))))
	start := time.Now()
	r := docker.Read([]string{f.Sock}, start.Add(2*time.Second))
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Errorf("took %v", took)
	}
	if r.Code != "deadline" || r.Containers != nil {
		t.Errorf("reading %+v, want none with deadline", r)
	}
}

// An id that is not 64 hexadecimal characters goes into no request: its container makes the reading
// nothing. Mutation: building the path from any id sends the stats request (3 requests, not 2).
func TestBudgetReadingOddIDIsNothing(t *testing.T) {
	for _, id := range []string{"abc", dockertest.ID('a')[:63], dockertest.ID('a') + "0", dockertest.ID('g'), dockertest.ID('a')[:63] + " ", dockertest.ID('a')[:62] + "\r\n", "../" + dockertest.ID('a')[3:]} {
		f := dockertest.Engine(t, dockertest.WithContainer(container(id)))
		r := docker.Read([]string{f.Sock}, dockertest.Soon())
		if r.Code != "parse" || r.Containers != nil || len(f.Requests()) != 2 {
			t.Errorf("id %q: reading %+v after %q, want none with parse and no stats request", id, r, f.Requests())
		}
	}
}

// One stats call that fails (a 500) voids the reading. Mutation: keeping the containers read so
// far, or skipping the failed one like a stopped container, turns this red.
func TestBudgetReadingOneStatsFailsIsNothing(t *testing.T) {
	f := dockertest.Engine(t, dockertest.StatsStatus(dockertest.ID('b'), 500),
		dockertest.WithContainer(container(dockertest.ID('a'))), dockertest.WithContainer(container(dockertest.ID('b'))))
	r := docker.Read([]string{f.Sock}, dockertest.Soon())
	if r.Code != "http" || r.Containers != nil {
		t.Errorf("reading %+v, want none with http", r)
	}
}

// A container that went between the list and its stats, removed (404) or stopped (200 with a limit
// of 0), costs only itself. Mutations: treating the zero limit as a reading lists the stopped one
// at 0 MiB; treating the 404 like any other status voids the reading.
func TestBudgetReadingStoppedContainerSkipped(t *testing.T) {
	f := dockertest.Engine(t, dockertest.StoppedAtStats(dockertest.ID('1')), dockertest.StatsStatus(dockertest.ID('2'), 404),
		dockertest.WithContainer(container(dockertest.ID('1'))), dockertest.WithContainer(container(dockertest.ID('2'))), dockertest.WithContainer(container(dockertest.ID('3'))))
	r := docker.Read([]string{f.Sock}, dockertest.Soon())
	if r.Code != "" || len(r.Containers) != 1 || r.Containers[0].ID != dockertest.ID('3') {
		t.Errorf("reading %+v, want only kept", r)
	}
}

// The list has its own limit, 4 MiB, above the 1 MiB of the others: one of 2 MiB is read, one over
// 4 MiB is no reading and goes no further. Mutations: a limit of 1 MiB reddens the first row; a limit
// of 16 MiB reads the padded list and goes on to the stats of its one entry, the third request.
func TestBudgetReadingListLimit(t *testing.T) {
	for _, c := range []struct {
		name     string
		pad      int
		code     string
		requests int
	}{{"2 MiB", 2 << 20, "", 3}, {"over 4 MiB", 4 << 20, "http", 2}} {
		t.Run(c.name, func(t *testing.T) {
			f := dockertest.Engine(t, dockertest.PadList(c.pad), dockertest.WithContainer(container(dockertest.ID('a'))))
			r := docker.Read([]string{f.Sock}, dockertest.Soon())
			if r.Code != c.code || (c.code == "" && len(r.Containers) != 1) || (c.code != "" && r.Containers != nil) || len(f.Requests()) != c.requests {
				t.Errorf("reading %+v after %q, want code %q after %d requests", r, f.Requests(), c.code, c.requests)
			}
		})
	}
}

// Another engine is a reading with no figures. Mutation: reading its containers and memory anyway.
func TestBudgetReadingOtherEngine(t *testing.T) {
	f := dockertest.Engine(t, dockertest.WithInfo("Ubuntu 24.04", 8<<30), dockertest.WithContainer(container(dockertest.ID('a'))))
	r := docker.Read([]string{f.Sock}, dockertest.Soon())
	if r.Code != "" || !r.OtherEngine || r.VMMiB != 0 || r.Containers != nil {
		t.Errorf("reading %+v, want OK, other engine and no figures", r)
	}
}

// The client asks and never changes anything. Mutation: a POST or a DELETE of any request turns it red.
func TestBudgetReadingSendsOnlyGet(t *testing.T) {
	f := dockertest.Engine(t, dockertest.WithContainer(container(dockertest.ID('a'))))
	if r := docker.Read([]string{f.Sock}, dockertest.Soon()); r.Code != "" {
		t.Fatalf("reading %+v", r)
	}
	stats, _ := docker.StatsPath(dockertest.ID('a'))
	if want := []string{"GET /info HTTP/1.0", "GET /containers/json HTTP/1.0", "GET " + stats + " HTTP/1.0"}; !reflect.DeepEqual(f.Requests(), want) {
		t.Errorf("requests %q, want %q", f.Requests(), want)
	}
}
