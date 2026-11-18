//go:build unix

package telemetry

import (
	"time"

	"github.com/core-slot/coreslot/internal/docker"
	"github.com/core-slot/coreslot/internal/store"
)

const (
	containersBudget = 2 * time.Second // the whole read, but for a connect resolving its path, which nothing interrupts
	containersMax    = 16
	imageMax         = 64         // an image reference is cut to this on a line
	tcpHost          = "tcp_host" // containers_err of a DOCKER_HOST that is not a unix socket
)

// ContainersAsync writes a containers line. off is the run's Docker gate (docker.Features): a
// remote DOCKER_HOST is a line carrying its code with nothing read, and any other reason is no line.
func (r *Recorder) ContainersAsync(sockets []string, off docker.Off) {
	if off != docker.On && off != docker.OffRemoteHost {
		return
	}
	r.machineLine(store.ContainersStamp, "containers", func() Fields {
		start := time.Now()
		var list []docker.Container
		var total int
		code := tcpHost
		if off == docker.On {
			list, total, code = Containers(start.Add(containersBudget), sockets)
		}
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
	sock, running, errCode := docker.List(sockets, deadline)
	if errCode != "" {
		return nil, 0, errCode
	}
	total = len(running)
	for _, l := range running[:min(total, containersMax)] {
		c, gone, code := docker.Stat(sock, l, deadline)
		if code != "" {
			errCode = code
			if code == docker.Deadline {
				return list, total, errCode
			}
			continue
		}
		if !gone {
			list = append(list, docker.Container{ID: docker.ShortID(c.ID), Image: c.Image[:min(imageMax, len(c.Image))],
				UsageMiB: c.UsageMiB, LimitMiB: c.LimitMiB})
		}
	}
	return list, total, errCode
}
