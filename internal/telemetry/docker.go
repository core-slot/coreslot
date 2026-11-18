//go:build unix

package telemetry

import (
	"encoding/json"
	"time"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/store"
)

const (
	containersBudget = 2 * time.Second // the whole read, but for a connect resolving its path, which nothing interrupts
	containersMax    = 16
	imageMax         = 64 // an image reference is cut to this on a line
)

// ContainersAsync writes a containers line.
func (r *Recorder) ContainersAsync(sockets []string) {
	r.machineLine(store.ContainersStamp, "containers", func() Fields {
		start := time.Now()
		list, total, code := Containers(start.Add(containersBudget), sockets)
		readMS := time.Since(start).Milliseconds()
		items := make([]Fields, 0, len(list))
		for _, c := range list {
			items = append(items, Fields{"id": c.ID, "image": c.Image, "usage_mib": c.UsageMiB, "limit_mib": c.LimitMiB})
		}
		f := Fields{"containers": items, "containers_total": total, "read_ms": readMS}
		if code != "" {
			f["containers_err"] = code
		}
		return f
	})
}

// Containers reads up to containersMax running containers from the first of sockets that answers,
// all of it against one deadline and with no retry. It returns what it read, how many are running
// and a closed code, "" for none. A container that cannot be read costs only itself; at the
// deadline the read ends with what it has.
func Containers(deadline time.Time, sockets []string) (list []docker.Container, total int, errCode string) {
	sock, body, errCode := docker.Walk(sockets, "/containers/json", deadline, docker.ResponseMax)
	if errCode != "" {
		return nil, 0, errCode
	}
	var running []struct {
		ID    string `json:"Id"`
		Image string `json:"Image"`
	}
	if json.Unmarshal(body, &running) != nil {
		return nil, 0, docker.Parse
	}
	total = len(running)
	for _, c := range running[:min(total, containersMax)] {
		var b []byte
		path, ok := docker.StatsPath(c.ID)
		code := docker.Parse // an id the engine does not make is read no further
		if ok {
			b, code = docker.Get(sock, path, deadline, docker.ResponseMax)
		}
		var s docker.Stats
		if code == "" && json.Unmarshal(b, &s) != nil {
			code = docker.Parse
		}
		if code != "" {
			errCode = code
			if code == docker.Deadline {
				return list, total, errCode
			}
			continue
		}
		if s.MemoryStats.Limit == 0 { // it stopped after the list; its zeros are no reading
			continue
		}
		list = append(list, docker.Container{ID: docker.ShortID(c.ID), Image: c.Image[:min(imageMax, len(c.Image))],
			UsageMiB: int64(docker.UsedBytes(s) >> 20), LimitMiB: int64(s.MemoryStats.Limit >> 20)})
	}
	return list, total, errCode
}
