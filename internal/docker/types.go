package docker

// DesktopOS is the one engine whose VM size is the budget's: any other is another engine.
const DesktopOS = "Docker Desktop"

// TCPHost stands for the one candidate socket when DOCKER_HOST is not a unix socket. It is also
// the code a reading then carries: nothing is connected to.
const TCPHost = "tcp_host"

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
}

// FreeEstMiB is what the VM has left by the containers' usage.
func (r Reading) FreeEstMiB() int64 {
	free := r.VMMiB
	for _, c := range r.Containers {
		free -= c.UsageMiB
	}
	return free
}
