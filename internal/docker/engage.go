package docker

import (
	"os"
	"strings"
)

// The notes a run carries in its hold's docker_note: why it has no forwarder, or why its Docker
// part is ungated. Each is at most 64 bytes of [a-z0-9:. -].
const (
	NoteNeedsEntry = "no-forwarder: needs-entry" // the needs file does not load, or the label's entry does not parse
	NoteSwitch     = "no-forwarder: switch"      // docker_forward = false
	NoteUpstream   = "no-forwarder: upstream"    // the command's socket is not Docker Desktop's, or Docker is not running
	NoteContext    = "no-forwarder: context"     // the user selected a Docker context other than Docker Desktop's
	NoteAddress    = "no-forwarder: address"     // the forwarder's directory or address cannot be had
	NoteStart      = "no-forwarder: start"       // the forwarder did not start
	NoteBlind      = "ungated: blind"            // the run lost sight of its containers
	NoteNoReading  = "ungated: no-reading"       // the engine could not be read
	NoteOversized  = "ungated: oversized"        // the Docker need does not fit the VM
)

// OffNote is the note of a run whose Docker features are off for off. OffPlatform has none: the
// features do not exist there, and Engage never says it.
func OffNote(off Off) string { return "no-forwarder: " + string(off) }

// EngageInput is what decides whether a run gets a forwarder. Off is the features' state for the
// run; Interest that its label qualifies (a container command, a container mark in its window, a
// Docker need); Switch the needs file's docker_forward (nil when absent); EntryBad that the needs
// file does not load or the label's entry does not parse. UserHost is the user's DOCKER_HOST (""
// when unset), Context the Docker context the user selected ("" for none). Reference is Docker
// Desktop's own socket; a test names its fake engine.
type EngageInput struct {
	Off       Off
	Interest  bool
	Switch    *bool
	EntryBad  bool
	UserHost  string
	Context   string
	Reference string
}

// Engage decides once whether a run gets a forwarder, and on which upstream. upstream != "" means
// engaged. Otherwise note is the reason for a run with a Docker interest, and "" for a run without
// one or on a platform without the features: such a run says nothing. The first reason wins: the
// features off, a bad needs entry, docker_forward = false; then the upstream must be one socket, the
// user's DOCKER_HOST or Docker Desktop's own, and the same file as Docker Desktop's; then, unless the
// user set DOCKER_HOST, the selected context must be none, default or desktop-linux. This withholds a
// forwarder; it never refuses a run.
func Engage(in EngageInput) (upstream, note string) {
	var why string
	switch {
	case in.Off == OffPlatform:
		return "", ""
	case in.Off != On:
		why = OffNote(in.Off)
	case in.EntryBad:
		why = NoteNeedsEntry
	case in.Switch != nil && !*in.Switch:
		why = NoteSwitch
	}
	if !in.Interest && (in.Switch == nil || !*in.Switch) {
		return "", ""
	}
	if why != "" {
		return "", why
	}
	upstream = in.Reference
	if in.UserHost != "" {
		path, ok := strings.CutPrefix(in.UserHost, "unix://")
		if !ok {
			return "", NoteUpstream
		}
		upstream = path
	}
	if !SameFile(upstream, in.Reference) {
		return "", NoteUpstream
	}
	if in.UserHost == "" && in.Context != "" && in.Context != "default" && in.Context != "desktop-linux" {
		return "", NoteContext
	}
	return upstream, ""
}

// SameFile reports whether a and b name the same file, by device and inode after following
// symlinks. A path that cannot be stated is the same file as nothing.
func SameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}
