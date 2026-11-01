//go:build coreslot_testhooks

// Package testhooks provides pause points, a wall-clock offset, a format list, a getent substitute,
// a password database that finds no record, a memory headroom and machine readings for the
// deterministic trace tests and the fallback tests.
// This file is the enabled build (tag coreslot_testhooks); the
// variables it reads are ignored by every other build.
//
//   - CORESLOT_TESTHOOK_DIR: directory for the pause handshake files.
//   - CORESLOT_TESTHOOK_PAUSE: comma-separated point names to pause at.
//   - CORESLOT_TESTHOOK_REALTIME_OFFSET: a Go duration added to the wall clock.
//   - <CORESLOT_TESTHOOK_DIR>/realtime-offset: when this file exists, its content (a Go
//     duration) replaces CORESLOT_TESTHOOK_REALTIME_OFFSET, re-read whenever the file changes,
//     so that a test steps the wall clock of every running process at once.
//   - CORESLOT_TESTHOOK_GETENT: the absolute path of a getent that replaces the system's in the
//     home directory's NSS fallback.
//   - CORESLOT_TESTHOOK_PASSWD_MISS: when set, the home directory's password-database lookup
//     finds no record for the uid, so that the NSS fallback runs on a host whose lookup would
//     answer (darwin's always does, and so does any account with a passwd entry).
//   - CORESLOT_TESTHOOK_HEADROOM_MIB: a decimal integer of MiB that replaces the kernel's memory
//     headroom, so that a test's admission never depends on the machine's memory.
//   - <CORESLOT_TESTHOOK_DIR>/readings: when this file exists, its named numbers replace the
//     machine readings of those names, re-read whenever the file changes.
package testhooks

import (
	"fmt"
	"maps"
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
		if d, ok := offsetCache.load(filepath.Join(dir, OffsetFile), time.ParseDuration); ok {
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

// ReadingsFile is the name, under CORESLOT_TESTHOOK_DIR, of the steppable machine readings.
const ReadingsFile = "readings"

// Readings returns the numbers of <CORESLOT_TESTHOOK_DIR>/readings, one "name value" line each,
// re-read whenever the file changes as the offset file is; set is false when it does not exist.
// The names are the caller's: it replaces the readings named and keeps the others.
func Readings() (nums map[string]int64, set bool) {
	dir := os.Getenv("CORESLOT_TESTHOOK_DIR")
	if dir == "" {
		return nil, false
	}
	nums, set = readingsCache.load(filepath.Join(dir, ReadingsFile), parseReadings)
	return maps.Clone(nums), set
}

// parseReadings parses the lines of a readings file.
func parseReadings(text string) (map[string]int64, error) {
	nums := map[string]int64{}
	for line := range strings.Lines(text) {
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, fmt.Errorf("want \"name value\", got %q", line)
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return nil, err
		}
		nums[f[0]] = n
	}
	return nums, nil
}

var (
	offsetCache   fileCache[time.Duration]
	readingsCache fileCache[map[string]int64]
)

// fileCache holds the last parse of a hook file, keyed by path, mtime and size.
type fileCache[T any] struct {
	sync.Mutex
	path  string
	mtime time.Time
	size  int64
	v     T
}

// load returns the parse of the file at path, parsed again only when its mtime or size changed;
// ok is false when it does not exist. A file that does not parse panics.
func (c *fileCache[T]) load(path string, parse func(string) (T, error)) (v T, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return v, false
	}
	c.Lock()
	defer c.Unlock()
	if c.path == path && c.mtime.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c.v, true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return v, false // removed between the stat and the read
	}
	if v, err = parse(strings.TrimSpace(string(data))); err != nil {
		panic(fmt.Sprintf("testhooks: %s: %v", path, err))
	}
	c.path, c.mtime, c.size, c.v = path, fi.ModTime(), fi.Size(), v
	return v, true
}

// Getent returns CORESLOT_TESTHOOK_GETENT, or "" (the system's getent) when it is unset.
func Getent() string { return os.Getenv("CORESLOT_TESTHOOK_GETENT") }

// PasswdMiss reports whether CORESLOT_TESTHOOK_PASSWD_MISS is set.
func PasswdMiss() bool { return os.Getenv("CORESLOT_TESTHOOK_PASSWD_MISS") != "" }

// Headroom returns CORESLOT_TESTHOOK_HEADROOM_MIB; set is false when it is unset.
func Headroom() (mib int64, set bool) {
	v := os.Getenv("CORESLOT_TESTHOOK_HEADROOM_MIB")
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		panic(fmt.Sprintf("testhooks: CORESLOT_TESTHOOK_HEADROOM_MIB: %v", err))
	}
	return n, true
}
