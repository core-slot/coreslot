//go:build unix

package docker_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
)

func soon() time.Time { return time.Now().Add(2 * time.Second) }

const longID = "0123456789abcdef0123456789abcdef"

// Both containers with the name's slash cut, the compose project, usage less inactive file and the
// limit, and the VM size in MiB. Mutations: a name kept with its slash, usage taken whole, or
// MemTotal read in bytes each redden the comparison.
func TestBudgetReadingComplete(t *testing.T) {
	sock := dockertest.Engine(t,
		dockertest.WithInfo(docker.DesktopOS, 27_268_218_880),
		dockertest.WithContainer(docker.Container{ID: longID, Name: "db", Image: "postgres:16", Project: "shop", UsageMiB: 400, LimitMiB: 2048}),
		dockertest.WithInactiveFile(longID, 100),
		dockertest.WithContainer(docker.Container{ID: "abc", Name: "cache", Image: "alpine", UsageMiB: 5, LimitMiB: 1024}))
	r := docker.Read([]string{sock}, soon())
	want := []docker.Container{
		{ID: longID[:12], Name: "db", Image: "postgres:16", Project: "shop", UsageMiB: 300, LimitMiB: 2048},
		{ID: "abc", Name: "cache", Image: "alpine", UsageMiB: 5, LimitMiB: 1024}}
	if !r.OK || r.Code != "" || r.OtherEngine || r.VMMiB != 26005 || !reflect.DeepEqual(r.Containers, want) || r.StartedNS == 0 {
		t.Errorf("reading %+v, want OK, VM 26005 and %+v", r, want)
	}
	if got := r.FreeEstMiB(); got != 26005-305 {
		t.Errorf("FreeEstMiB %d, want %d", got, 26005-305)
	}
}

// A hung engine is no reading, and within the one deadline. Mutation: a deadline per call, or a
// read in the caller without one, outlasts 2.5 s.
func TestBudgetReadingHungEngineIsNothing(t *testing.T) {
	sock := dockertest.Engine(t, dockertest.Hang())
	start := time.Now()
	r := docker.Read([]string{sock}, soon())
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Errorf("took %v", took)
	}
	if r.OK || r.Code != "deadline" || r.Containers != nil {
		t.Errorf("reading %+v, want none with deadline", r)
	}
}

// One failed stats call voids the reading. Mutation: keeping the containers read so far, or
// skipping the failed one like a stopped container, turns this red.
func TestBudgetReadingOneStatsFailsIsNothing(t *testing.T) {
	sock := dockertest.Engine(t, dockertest.FailStatsOnCall(2),
		dockertest.WithContainer(docker.Container{ID: "a", Name: "a", Image: "x", UsageMiB: 10, LimitMiB: 100}),
		dockertest.WithContainer(docker.Container{ID: "b", Name: "b", Image: "x", UsageMiB: 10, LimitMiB: 100}))
	r := docker.Read([]string{sock}, soon())
	if r.OK || r.Code != "http" || r.Containers != nil {
		t.Errorf("reading %+v, want none with http", r)
	}
}

// A container that stopped between the list and its stats costs only itself. Mutation: treating
// its zero limit as a reading lists it with 0 MiB.
func TestBudgetReadingStoppedContainerSkipped(t *testing.T) {
	sock := dockertest.Engine(t, dockertest.StopBetweenListAndStats("gone"),
		dockertest.WithContainer(docker.Container{ID: "gone", Name: "gone", Image: "x", UsageMiB: 10, LimitMiB: 100}),
		dockertest.WithContainer(docker.Container{ID: "kept", Name: "kept", Image: "x", UsageMiB: 10, LimitMiB: 100}))
	r := docker.Read([]string{sock}, soon())
	if !r.OK || len(r.Containers) != 1 || r.Containers[0].ID != "kept" {
		t.Errorf("reading %+v, want only kept", r)
	}
}

// A list over 4 MiB is no reading. Mutation: a list limit of 16 MiB reads the padded list and
// goes on to the stats of its one entry, the third request.
func TestBudgetReadingListOver4MiBIsNothing(t *testing.T) {
	d := dockertest.EngineFake(t, dockertest.HugeList())
	r := docker.Read([]string{d.Path}, soon())
	if r.OK || r.Code != "http" || r.Containers != nil {
		t.Errorf("reading %+v, want none with http", r)
	}
	if n := len(d.Requests()); n != 2 {
		t.Errorf("%d requests, want /info and the list only", n)
	}
}

// A list of 2 MiB is read: the list has its own limit, above the 1 MiB of the others. Mutation:
// reading the list with the 1 MiB limit turns this red.
func TestBudgetReadingListOf2MiBIsRead(t *testing.T) {
	sock := dockertest.Engine(t, dockertest.PadList(2<<20),
		dockertest.WithContainer(docker.Container{ID: "a", Name: "a", Image: "x", UsageMiB: 10, LimitMiB: 100}))
	r := docker.Read([]string{sock}, soon())
	if !r.OK || len(r.Containers) != 1 {
		t.Errorf("reading %+v, want the one container", r)
	}
}

// Another engine is a reading with no figures. Mutation: reading its containers and memory anyway.
func TestBudgetReadingOtherEngine(t *testing.T) {
	sock := dockertest.Engine(t, dockertest.WithInfo("Ubuntu 24.04", 8<<30),
		dockertest.WithContainer(docker.Container{ID: "a", Name: "a", Image: "x", UsageMiB: 10, LimitMiB: 100}))
	r := docker.Read([]string{sock}, soon())
	if !r.OK || !r.OtherEngine || r.VMMiB != 0 || r.Containers != nil {
		t.Errorf("reading %+v, want OK, other engine and no figures", r)
	}
}

// The client asks and never changes anything. Mutation: a POST or a DELETE of any request turns it red.
func TestBudgetReadingSendsOnlyGet(t *testing.T) {
	d := dockertest.EngineFake(t, dockertest.WithContainer(docker.Container{ID: "a", Name: "a", Image: "x", UsageMiB: 10, LimitMiB: 100}))
	if r := docker.Read([]string{d.Path}, soon()); !r.OK {
		t.Fatalf("reading %+v", r)
	}
	reqs := d.Requests()
	if len(reqs) != 3 {
		t.Errorf("requests %q, want /info, the list and one stats", reqs)
	}
	for _, line := range reqs {
		if !strings.HasPrefix(line, "GET ") {
			t.Errorf("request %q is not a GET", line)
		}
	}
}

// The socket does not reach a child: a command that inherited it would read as a container client.
// It is non-blocking, or no deadline bounds it. Mutations: no CloseOnExec; no SetNonblock.
func TestSocketIsCloseOnExec(t *testing.T) {
	sock := dockertest.Engine(t)
	fd, code := docker.Connect(sock)
	if code != "" {
		t.Fatalf("code %q", code)
	}
	defer unix.Close(fd)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Errorf("F_GETFD %#x, %v: not close-on-exec", flags, err)
	}
	fl, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil || fl&unix.O_NONBLOCK == 0 {
		t.Errorf("F_GETFL %#x, %v: blocking", fl, err)
	}
}
