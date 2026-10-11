//go:build coreslot_testhooks

// Package testhooks provides pause points, a wall-clock offset and a format list for the
// deterministic trace tests. This file is the enabled build (tag coreslot_testhooks); the
// variables it reads are ignored by every other build.
//
//   - CORESLOT_TESTHOOK_DIR: directory for the pause handshake files.
//   - CORESLOT_TESTHOOK_PAUSE: comma-separated point names to pause at.
//   - CORESLOT_TESTHOOK_REALTIME_OFFSET: a Go duration added to the wall clock.
//   - CORESLOT_TESTHOOK_FORMATS: comma-separated formats the client implements.
package testhooks

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const pollInterval = 100 * time.Millisecond

// Point pauses when CORESLOT_TESTHOOK_DIR is set and name is listed in CORESLOT_TESTHOOK_PAUSE:
// it creates <dir>/<pid>.<name>.reached, waits until <dir>/<pid>.<name>.go exists, then removes
// both and returns.
func Point(name string) {
	dir := os.Getenv("CORESLOT_TESTHOOK_DIR")
	if dir == "" || !slices.Contains(strings.Split(os.Getenv("CORESLOT_TESTHOOK_PAUSE"), ","), name) {
		return
	}
	prefix := filepath.Join(dir, fmt.Sprintf("%d.%s", os.Getpid(), name))
	if err := os.WriteFile(prefix+".reached", nil, 0o600); err != nil {
		panic(fmt.Sprintf("testhooks: %v", err))
	}
	for {
		if _, err := os.Stat(prefix + ".go"); err == nil {
			break
		}
		time.Sleep(pollInterval)
	}
	for _, suffix := range []string{".reached", ".go"} {
		if err := os.Remove(prefix + suffix); err != nil {
			panic(fmt.Sprintf("testhooks: %v", err))
		}
	}
}

// RealtimeOffset returns CORESLOT_TESTHOOK_REALTIME_OFFSET, or 0 when it is unset.
func RealtimeOffset() time.Duration {
	v := os.Getenv("CORESLOT_TESTHOOK_REALTIME_OFFSET")
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		panic(fmt.Sprintf("testhooks: CORESLOT_TESTHOOK_REALTIME_OFFSET: %v", err))
	}
	return d
}

// Formats returns CORESLOT_TESTHOOK_FORMATS, or [1] when it is unset.
func Formats() []int {
	v := os.Getenv("CORESLOT_TESTHOOK_FORMATS")
	if v == "" {
		return []int{1}
	}
	var formats []int
	for _, f := range strings.Split(v, ",") {
		n, err := strconv.Atoi(f)
		if err != nil {
			panic(fmt.Sprintf("testhooks: CORESLOT_TESTHOOK_FORMATS: %v", err))
		}
		formats = append(formats, n)
	}
	return formats
}
