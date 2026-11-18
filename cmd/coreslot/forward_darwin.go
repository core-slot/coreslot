//go:build darwin

package main

import (
	"os"

	relay "github.com/core-slot/coreslot/internal/forward"
)

// hasForward says that this build has the forward subcommand, which only an engaged run starts.
const hasForward = true

// serveForward is the forwarder's work; a test replaces it.
var serveForward = relay.Serve

// forwardCommand is `coreslot forward <entry> <upstream>`, the forwarder a run starts for itself:
// its descriptor 3 is the read end of the pipe whose end is the run's end, and 4 the write end of
// the pipe of its events to the run. It prints nothing, and its status says only whether it served.
func forwardCommand(args []string) int {
	if len(args) != 2 {
		return exitUsage
	}
	err := serveForward(relay.Config{Entry: args[0], Upstream: args[1], End: os.NewFile(3, "end"), Events: os.NewFile(4, "events")})
	if err != nil {
		return exitInternal
	}
	return 0
}
