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
	if len(sockets) == 1 && sockets[0] == docker.TCPHost {
		return nil, 0, docker.TCPHost
	}
	var sock string
	var body []byte
	errCode = docker.NoSocket
	for _, s := range sockets {
		b, code := docker.Get(s, "/containers/json", deadline, docker.ResponseMax)
		if code == docker.NoSocket {
			continue
		}
		errCode = code
		if code != docker.ConnectErr {
			sock, body = s, b
			break
		}
	}
	if sock == "" || errCode != "" {
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
		b, code := docker.Get(sock, docker.StatsPath(c.ID), deadline, docker.ResponseMax)
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
		list = append(list, docker.Container{ID: docker.ShortID(c.ID), Image: docker.ShortImage(c.Image),
			UsageMiB: int64(docker.UsedBytes(s) >> 20), LimitMiB: int64(s.MemoryStats.Limit >> 20)})
	}
	return list, total, errCode
}
