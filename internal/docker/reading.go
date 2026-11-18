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
// answer, or the code of the last failure. DOCKER_HOST over TCP is a code, with no connection.
func Walk(sockets []string, target string, deadline time.Time, max int64) (sock string, body []byte, code string) {
	if len(sockets) == 1 && sockets[0] == TCPHost {
		return "", nil, TCPHost
	}
	code = NoSocket
	for _, s := range sockets {
		b, c := Get(s, target, deadline, max)
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

// Read reads the engine's memory and every running container's usage from the first of sockets
// that connects (Walk), all against one deadline and with no retry. Any failed call is no reading,
// and so is a container whose id is not one the engine makes. A container that stopped between the
// list and its stats is left out: one removed (404) or stopped (a limit of 0). Any other status is
// no reading.
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
		path, ok := StatsPath(c.ID)
		if !ok {
			r.Code = Parse
			return r
		}
		b, status, code := do(sock, path, deadline, ResponseMax)
		if status == 404 { // removed after the list, as `docker run --rm` does
			continue
		}
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
		list = append(list, Container{ID: c.ID, Name: name, Image: c.Image, Project: c.Labels[composeKey],
			UsageMiB: int64(UsedBytes(s) >> 20), LimitMiB: int64(s.MemoryStats.Limit >> 20)})
	}
	r.OK, r.VMMiB, r.Containers = true, int64(in.MemTotal>>20), list
	return r
}
