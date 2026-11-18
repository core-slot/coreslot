package docker

// DesktopOS is the one engine whose VM size is the budget's: any other is another engine.
const DesktopOS = "Docker Desktop"

// Container is one running container as `docker stats` shows it. Its strings are the engine's, whole:
// the id is the full 64 characters, and whoever shows one cuts it (ShortID).
type Container struct {
	ID, Name, Image, Project string
	UsageMiB, LimitMiB       int64
}

// Reading is a budget reading of the engine: all of it or nothing. Code is set when any call failed. OtherEngine is an engine that is not Docker Desktop: no figures.
type Reading struct {
	Code        string
	OtherEngine bool
	VMMiB       int64 // /info's MemTotal
	Containers  []Container
}
