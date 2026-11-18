//go:build unix

package docker

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	ResponseMax = 1 << 20 // /info and a container's stats
	listMax     = 4 << 20 // the container list: a name and labels each
	composeKey  = "com.docker.compose.project"
)

// DesktopOS is the one engine whose VM size is the budget's: any other is another engine.
const DesktopOS = "Docker Desktop"

// Container is one running container as `docker stats` shows it. Its strings are the engine's, whole:
// the id is the full 64 characters, and whoever shows one cuts it (ShortID).
type Container struct {
	ID, Name, Image, Project string
	UsageMiB, LimitMiB       int64
}

// Reading is a budget reading of the engine: all of it or nothing. Code is set when any call failed.
// OtherEngine is an engine that is not Docker Desktop: no figures.
type Reading struct {
	Code        string
	OtherEngine bool
	VMMiB       int64 // /info's MemTotal
	Containers  []Container
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

// StatsPath is the request for one container's stats, once. An id is the engine's own text, so it
// is put in a request only when it is 64 hexadecimal characters.
func StatsPath(id string) (string, bool) {
	if len(id) != 64 || strings.Trim(id, "0123456789abcdefABCDEF") != "" {
		return "", false
	}
	return "/containers/" + id + "/stats?stream=false&one-shot=true", true
}

// Walk makes the request target on the first of sockets that connects, against one deadline: no_socket
// and connect move on to the next candidate. It returns that socket with the body and the code of the
// answer, or the code of the last failure.
func Walk(sockets []string, target string, deadline time.Time, max int64) (sock string, body []byte, code string) {
	code = NoSocket
	for _, s := range sockets {
		b, _, c := Get(s, target, deadline, max)
		if c == NoSocket {
			continue
		}
		code = c
		if c != ConnectErr {
			return s, b, c
		}
	}
	return "", nil, code
}

// Listed is a running container as the engine's list gives it.
type Listed struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

// List reads the running containers from the first of sockets that answers (Walk), and returns that
// socket.
func List(sockets []string, deadline time.Time) (sock string, running []Listed, code string) {
	sock, body, code := Walk(sockets, "/containers/json", deadline, listMax)
	if code == "" && json.Unmarshal(body, &running) != nil {
		code = Parse
	}
	return sock, running, code
}

// Stat reads one listed container's stats. The id goes into a request only when StatsPath accepts
// it, else the code is Parse. gone is a container that stopped between the list and its stats,
// removed (404) or stopped (a limit of 0, whose zeros are no reading): it is no container and no
// code. Any other failure is its code.
func Stat(sock string, l Listed, deadline time.Time) (c Container, gone bool, code string) {
	path, ok := StatsPath(l.ID)
	if !ok {
		return c, false, Parse
	}
	b, status, code := Get(sock, path, deadline, ResponseMax)
	if status == 404 {
		return c, true, ""
	}
	var s Stats
	if code == "" && json.Unmarshal(b, &s) != nil {
		code = Parse
	}
	if code != "" {
		return c, false, code
	}
	if s.MemoryStats.Limit == 0 {
		return c, true, ""
	}
	if len(l.Names) > 0 {
		c.Name = strings.TrimPrefix(l.Names[0], "/")
	}
	c.ID, c.Image, c.Project = l.ID, l.Image, l.Labels[composeKey]
	c.UsageMiB, c.LimitMiB = int64(UsedBytes(s)>>20), int64(s.MemoryStats.Limit>>20)
	return c, false, ""
}

// Read reads the engine's memory and every running container's usage from the first of sockets
// that connects (Walk), all against one deadline and with no retry. Any failed call is no reading,
// and so is a container whose id is not one the engine makes. A container that is gone (Stat) is
// left out.
func Read(sockets []string, deadline time.Time) Reading {
	var r Reading
	sock, info, code := Walk(sockets, "/info", deadline, ResponseMax)
	if code != "" {
		r.Code = code
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
		r.OtherEngine = true
		return r
	}
	_, running, code := List([]string{sock}, deadline)
	if code != "" {
		r.Code = code
		return r
	}
	var list []Container
	for _, l := range running {
		c, gone, code := Stat(sock, l, deadline)
		if code != "" {
			r.Code = code
			return r
		}
		if !gone {
			list = append(list, c)
		}
	}
	r.VMMiB, r.Containers = int64(in.MemTotal>>20), list
	return r
}
