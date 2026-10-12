//go:build coreslot_testhooks

// Package testhooks provides pause points, a wall-clock offset, a format list, a getent substitute
// and the CLI's descriptor-limit probe for the deterministic trace tests and the fallback tests.
// This file is the enabled build (tag coreslot_testhooks); the
// variables it reads are ignored by every other build.
//
//   - CORESLOT_TESTHOOK_DIR: directory for the pause handshake files.
//   - CORESLOT_TESTHOOK_PAUSE: comma-separated point names to pause at.
//   - CORESLOT_TESTHOOK_REALTIME_OFFSET: a Go duration added to the wall clock.
//   - <CORESLOT_TESTHOOK_DIR>/realtime-offset: when this file exists, its content (a Go
//     duration) replaces CORESLOT_TESTHOOK_REALTIME_OFFSET, re-read whenever the file changes,
//     so that a test steps the wall clock of every running process at once.
//   - CORESLOT_TESTHOOK_FORMATS: comma-separated formats the client implements.
//   - CORESLOT_TESTHOOK_GETENT: the absolute path of a getent that replaces the system's in the
//     home directory's NSS fallback.
//   - CORESLOT_TESTHOOK_PROBE_SHELL: the program the CLI runs as `<it> -c 'ulimit -S -n'` to
//     learn the descriptor limit it was started with, so that a test can make that probe fail.
package testhooks

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OffsetFile is the name, under CORESLOT_TESTHOOK_DIR, of the steppable wall-clock offset.
const OffsetFile = "realtime-offset"

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

// RealtimeOffset returns the wall-clock offset: the content of <CORESLOT_TESTHOOK_DIR>/realtime-offset
// when that file exists, otherwise CORESLOT_TESTHOOK_REALTIME_OFFSET, otherwise 0. The file is
// stat'ed on every call and parsed again only when its mtime or size changed, so a test that
// rewrites it (through a rename) steps the clock of every running process that shares the
// directory.
func RealtimeOffset() time.Duration {
	if dir := os.Getenv("CORESLOT_TESTHOOK_DIR"); dir != "" {
		if d, ok := fileOffset(filepath.Join(dir, OffsetFile)); ok {
			return d
		}
	}
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

// offsetCache holds the last parsed offset file, keyed by path, mtime and size.
var offsetCache struct {
	sync.Mutex
	path  string
	mtime time.Time
	size  int64
	d     time.Duration
}

// fileOffset reads the offset file at path; ok is false when it does not exist.
func fileOffset(path string) (time.Duration, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	c := &offsetCache
	c.Lock()
	defer c.Unlock()
	if c.path == path && c.mtime.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c.d, true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false // removed between the stat and the read
	}
	d, err := time.ParseDuration(strings.TrimSpace(string(data)))
	if err != nil {
		panic(fmt.Sprintf("testhooks: %s: %v", path, err))
	}
	c.path, c.mtime, c.size, c.d = path, fi.ModTime(), fi.Size(), d
	return d, true
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

// Getent returns CORESLOT_TESTHOOK_GETENT, or "" (the system's getent) when it is unset.
func Getent() string { return os.Getenv("CORESLOT_TESTHOOK_GETENT") }

// ProbeShell returns CORESLOT_TESTHOOK_PROBE_SHELL, or /bin/sh when it is unset.
func ProbeShell() string {
	if v := os.Getenv("CORESLOT_TESTHOOK_PROBE_SHELL"); v != "" {
		return v
	}
	return "/bin/sh"
}
