package main

import (
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/Nezhinskiy/coreslot/internal/testhooks"
)

// versionCommand prints the CLI's version and the protocol formats it implements (spec section
// 6.2), for example `coreslot 0.1.0 (protocol formats: 1)`.
func versionCommand(args []string, io IO) int {
	if len(args) > 0 {
		fmt.Fprintf(io.Stderr, "coreslot: version takes no arguments\n")
		return exitUsage
	}
	formats := testhooks.Formats()
	text := make([]string, len(formats))
	for i, f := range formats {
		text[i] = strconv.Itoa(f)
	}
	fmt.Fprintf(io.Stdout, "coreslot %s (protocol formats: %s)\n", cliVersion(), strings.Join(text, ", "))
	return 0
}

// cliVersion is the version -ldflags set, or else the main module's version from the binary's
// build information, so `go install github.com/Nezhinskiy/coreslot/cmd/coreslot@v0.1.0` prints
// 0.1.0 rather than 0.0.0-dev.
func cliVersion() string {
	info, ok := debug.ReadBuildInfo()
	return resolveVersion(version, info, ok)
}

// resolveVersion keeps a version the build set; otherwise it takes the main module's version
// without its leading "v" (a module version, or the pseudo-version a build in a checkout
// stamps), and keeps the development default when the build recorded none ("(devel)").
func resolveVersion(set string, info *debug.BuildInfo, ok bool) string {
	if set != devVersion || !ok || info == nil {
		return set
	}
	v := info.Main.Version
	if v == "" || v == "(devel)" {
		return set
	}
	return strings.TrimPrefix(v, "v")
}
