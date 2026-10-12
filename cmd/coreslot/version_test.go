package main

import (
	"runtime/debug"
	"testing"
)

// A version set with -ldflags wins; without one, `go install module@vX` builds report the module
// version from the build information, and a build that recorded none keeps the development default.
func TestResolveVersion(t *testing.T) {
	info := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/Nezhinskiy/coreslot", Version: v}}
	}
	for _, c := range []struct {
		name, set string
		info      *debug.BuildInfo
		ok        bool
		want      string
	}{
		{"ldflags wins over the module version", "0.2.0", info("v0.1.0"), true, "0.2.0"},
		{"go install of a tag", devVersion, info("v0.1.0"), true, "0.1.0"},
		{"a pseudo-version", devVersion, info("v0.0.0-20261012020000-0123456789ab"), true, "0.0.0-20261012020000-0123456789ab"},
		{"no version recorded", devVersion, info("(devel)"), true, devVersion},
		{"empty version", devVersion, info(""), true, devVersion},
		{"no build information", devVersion, nil, false, devVersion},
	} {
		if got := resolveVersion(c.set, c.info, c.ok); got != c.want {
			t.Errorf("%s: resolveVersion = %q, want %q", c.name, got, c.want)
		}
	}
}
