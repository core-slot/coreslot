//go:build coreslot_testhooks && darwin

package main

import (
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/docker/dockertest"
)

// An engaged run whose command touched one container of two makes that container's calls only:
// one inspect of the name it touched, then stats by its id at tick 3, nothing for the other and no
// listing of the engine; its hold carries the footprint, 0 with no growth. Mutation: the sample
// reads every running container (docker.Read on the upstream), and the other's stats and the
// listing appear, red.
func TestEngagedRunReadsOnlyItsContainers(t *testing.T) {
	mine, other := dockertest.ID('a'), dockertest.ID('b')
	b := newFwBed(t, forced("fw"),
		dockertest.WithContainer(dockertest.Container{ID: mine, Name: "mine", UsageBytes: 100 << 20, LimitBytes: 1 << 30}),
		dockertest.WithContainer(dockertest.Container{ID: other, Name: "other", UsageBytes: 100 << 20, LimitBytes: 1 << 30}))
	touch := `curl -s -X POST --unix-socket "${DOCKER_HOST#unix://}" http://docker/containers/mine/restart; `
	if r := b.run(t, "fw", touch+`sleep 5.5; `+saveHold); r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	path, _ := docker.StatsPath(mine)
	var gets, stats []string
	for _, r := range b.fake.Requests() {
		if strings.HasPrefix(r, "GET ") {
			gets = append(gets, r)
		}
		if r == "GET "+path {
			stats = append(stats, r)
		}
	}
	if len(gets) == 0 || gets[0] != "GET /containers/mine/json" || len(stats) == 0 || len(stats) != len(gets)-1 {
		t.Errorf("the run's calls %q, want one inspect of mine, then its stats by id alone", gets)
	}
	if f, ok := b.hold(t)["docker_footprint_mib"]; !ok || f != 0.0 {
		t.Errorf("hold docker_footprint_mib %v (present %v), want 0", f, ok)
	}
}
