//go:build unix

package docker

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/core-slot/coreslot/internal/store"
)

const (
	// DesktopOS is the one engine whose VM size is the budget's: any other is another engine.
	DesktopOS = "Docker Desktop"

	ResponseMax = 1 << 20 // /info and a container's stats
	listMax     = 4 << 20 // the container list: a name and labels each
	imageMax    = 64
	composeKey  = "com.docker.compose.project"
)

// Container is one running container as `docker stats` shows it.
type Container struct {
	ID, Name, Image, Project string
	UsageMiB, LimitMiB       int64
}

// Reading is a budget reading of the engine: all of it or nothing. OK is false, with Code, when
// any call failed. OtherEngine is an engine that is not Docker Desktop: no figures.
type Reading struct {
	OK          bool
	Code        string
	OtherEngine bool
	VMMiB       int64 // /info's MemTotal
	Containers  []Container
	StartedNS   int64 // the instant the first request was sent
}

// FreeEstMiB is what the VM has left by the containers' usage.
func (r Reading) FreeEstMiB() int64 {
	free := r.VMMiB
	for _, c := range r.Containers {
		free -= c.UsageMiB
	}
	return free
}

// Stats is the part of a container's stats the readings use.
type Stats struct {
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

// UsedBytes is the usage `docker stats` shows: inactive file pages are left out, under the name of
// cgroup v1 and then of v2, only when they are smaller than the usage.
func UsedBytes(s Stats) uint64 {
	m := s.MemoryStats
	if v, ok := m.Stats["total_inactive_file"]; ok && v < m.Usage {
		return m.Usage - v
	}
	if v := m.Stats["inactive_file"]; v < m.Usage {
		return m.Usage - v
	}
	return m.Usage
}

// ShortID cuts an id to the 12 characters the CLI shows.
func ShortID(id string) string { return id[:min(12, len(id))] }

// ShortImage cuts an image reference to the length a line keeps.
func ShortImage(image string) string { return image[:min(imageMax, len(image))] }

// StatsPath is the request for one container's stats, once.
func StatsPath(id string) string { return "/containers/" + id + "/stats?stream=false&one-shot=true" }

// Read reads the engine's memory and every running container's usage from the first of sockets
// that connects, all against one deadline and with no retry; no_socket and connect move on to the
// next. Any failed call is no reading. A container that stopped between the list and its stats
// is left out.
func Read(sockets []string, deadline time.Time) Reading {
	r := Reading{StartedNS: store.NowNS()}
	if len(sockets) == 1 && sockets[0] == TCPHost {
		r.Code = TCPHost
		return r
	}
	var sock string
	var info []byte
	r.Code = NoSocket
	for _, s := range sockets {
		b, code := Get(s, "/info", deadline, ResponseMax)
		if code == NoSocket {
			continue
		}
		r.Code = code
		if code != ConnectErr {
			sock, info = s, b
			break
		}
	}
	if sock == "" || r.Code != "" {
		return r
	}
	var in struct {
		OperatingSystem string
		MemTotal        uint64
	}
	if json.Unmarshal(info, &in) != nil {
		r.Code = Parse
		return r
	}
	if in.OperatingSystem != DesktopOS {
		r.OK, r.OtherEngine = true, true
		return r
	}
	body, code := Get(sock, "/containers/json", deadline, listMax)
	if code != "" {
		r.Code = code
		return r
	}
	var running []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	}
	if json.Unmarshal(body, &running) != nil {
		r.Code = Parse
		return r
	}
	var list []Container
	for _, c := range running {
		b, code := Get(sock, StatsPath(c.ID), deadline, ResponseMax)
		var s Stats
		if code == "" && json.Unmarshal(b, &s) != nil {
			code = Parse
		}
		if code != "" {
			r.Code = code
			return r
		}
		if s.MemoryStats.Limit == 0 { // it stopped after the list; its zeros are no reading
			continue
		}
		var name string
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		list = append(list, Container{ID: ShortID(c.ID), Name: name, Image: ShortImage(c.Image), Project: c.Labels[composeKey],
			UsageMiB: int64(UsedBytes(s) >> 20), LimitMiB: int64(s.MemoryStats.Limit >> 20)})
	}
	r.OK, r.VMMiB, r.Containers = true, int64(in.MemTotal>>20), list
	return r
}
