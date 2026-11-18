//go:build unix

package docker_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
)

func soon() time.Time { return time.Now().Add(2 * time.Second) }

const (
	longID = "0123456789abcdef0123456789abcdef"
	mib    = 1 << 20
)

func container(id string) dockertest.Container {
	return dockertest.Container{ID: id, Name: id, Image: "x", UsageBytes: 10 * mib, LimitBytes: 100 * mib}
}

// Both containers with the name's slash cut, the compose project, usage less inactive file and the
// limit, and the VM size in MiB. Mutations: a name kept with its slash, usage taken whole, or
// MemTotal read in bytes each redden the comparison.
func TestBudgetReadingComplete(t *testing.T) {
	f := dockertest.Engine(t,
		dockertest.WithInfo(docker.DesktopOS, 27_268_218_880),
		dockertest.WithContainer(dockertest.Container{ID: longID, Name: "db", Image: "postgres:16", Project: "shop",
			UsageBytes: 400 * mib, InactiveFileBytes: 100 * mib, LimitBytes: 2048 * mib}),
		dockertest.WithContainer(dockertest.Container{ID: "abc", Name: "cache", Image: "alpine", UsageBytes: 5 * mib, LimitBytes: 1024 * mib}))
	r := docker.Read([]string{f.Sock}, soon())
	want := []docker.Container{
		{ID: longID[:12], Name: "db", Image: "postgres:16", Project: "shop", UsageMiB: 300, LimitMiB: 2048},
		{ID: "abc", Name: "cache", Image: "alpine", UsageMiB: 5, LimitMiB: 1024}}
	if !r.OK || r.Code != "" || r.OtherEngine || r.VMMiB != 26005 || !reflect.DeepEqual(r.Containers, want) {
		t.Errorf("reading %+v, want OK, VM 26005 and %+v", r, want)
	}
	if got := r.FreeEstMiB(); got != 26005-305 {
		t.Errorf("FreeEstMiB %d, want %d", got, 26005-305)
	}
}

// A hung engine is no reading, and within the one deadline. Mutation: a deadline per call, or a
// read in the caller without one, outlasts 2.5 s.
func TestBudgetReadingHungEngineIsNothing(t *testing.T) {
	f := dockertest.Engine(t, dockertest.Hang())
	start := time.Now()
	r := docker.Read([]string{f.Sock}, soon())
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Errorf("took %v", took)
	}
	if r.OK || r.Code != "deadline" || r.Containers != nil {
		t.Errorf("reading %+v, want none with deadline", r)
	}
}

// One stats call that fails (a 500) voids the reading. Mutation: keeping the containers read so
// far, or skipping the failed one like a stopped container, turns this red.
func TestBudgetReadingOneStatsFailsIsNothing(t *testing.T) {
	f := dockertest.Engine(t, dockertest.StatsStatus("b", 500),
		dockertest.WithContainer(container("a")), dockertest.WithContainer(container("b")))
	r := docker.Read([]string{f.Sock}, soon())
	if r.OK || r.Code != "http" || r.Containers != nil {
		t.Errorf("reading %+v, want none with http", r)
	}
}

// A container that went between the list and its stats, removed (404) or stopped (200 with a limit
// of 0), costs only itself. Mutations: treating the zero limit as a reading lists the stopped one
// at 0 MiB; treating the 404 like any other status voids the reading.
func TestBudgetReadingStoppedContainerSkipped(t *testing.T) {
	f := dockertest.Engine(t, dockertest.StoppedAtStats("stopped"), dockertest.StatsStatus("removed", 404),
		dockertest.WithContainer(container("stopped")), dockertest.WithContainer(container("removed")), dockertest.WithContainer(container("kept")))
	r := docker.Read([]string{f.Sock}, soon())
	if !r.OK || len(r.Containers) != 1 || r.Containers[0].ID != "kept" {
		t.Errorf("reading %+v, want only kept", r)
	}
}

// A list over 4 MiB is no reading. Mutation: a list limit of 16 MiB reads the padded list and
// goes on to the stats of its one entry, the third request.
func TestBudgetReadingListOver4MiBIsNothing(t *testing.T) {
	f := dockertest.Engine(t, dockertest.HugeList())
	r := docker.Read([]string{f.Sock}, soon())
	if r.OK || r.Code != "http" || r.Containers != nil {
		t.Errorf("reading %+v, want none with http", r)
	}
	if n := len(f.Requests()); n != 2 {
		t.Errorf("%d requests, want /info and the list only", n)
	}
}

// A list of 2 MiB is read: the list has its own limit, above the 1 MiB of the others. Mutation:
// reading the list with the 1 MiB limit turns this red.
func TestBudgetReadingListOf2MiBIsRead(t *testing.T) {
	f := dockertest.Engine(t, dockertest.PadList(2<<20), dockertest.WithContainer(container("a")))
	r := docker.Read([]string{f.Sock}, soon())
	if !r.OK || len(r.Containers) != 1 {
		t.Errorf("reading %+v, want the one container", r)
	}
}

// Another engine is a reading with no figures. Mutation: reading its containers and memory anyway.
func TestBudgetReadingOtherEngine(t *testing.T) {
	f := dockertest.Engine(t, dockertest.WithInfo("Ubuntu 24.04", 8<<30), dockertest.WithContainer(container("a")))
	r := docker.Read([]string{f.Sock}, soon())
	if !r.OK || !r.OtherEngine || r.VMMiB != 0 || r.Containers != nil {
		t.Errorf("reading %+v, want OK, other engine and no figures", r)
	}
}

// The client asks and never changes anything. Mutation: a POST or a DELETE of any request turns it red.
func TestBudgetReadingSendsOnlyGet(t *testing.T) {
	f := dockertest.Engine(t, dockertest.WithContainer(container("a")))
	if r := docker.Read([]string{f.Sock}, soon()); !r.OK {
		t.Fatalf("reading %+v", r)
	}
	if got := f.Methods(); !reflect.DeepEqual(got, []string{"GET", "GET", "GET"}) {
		t.Errorf("methods %q, want three GETs: /info, the list, one stats", got)
	}
	if slices.Contains(f.Methods(), "POST") {
		t.Error("a POST")
	}
}
