//go:build unix

package main

import (
	"maps"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/store"
)

// environ is the environment as a map; a later duplicate wins, as for exec.
func environ() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// decide decides the run's context from its inherited environment (PROTOCOL.md section 18; an
// empty variable is unset): an inherited CORESLOT_UNBUDGETED passes through unbudgeted, an
// inherited CORESLOT_TOKEN_FDS passes through on the parent's grant or refuses when it does not
// validate, and anything else, or --independent, is an ordinary request that drops the
// inherited context first.
func (r *run) decide() int {
	env := environ()
	if !r.o.independent {
		if reason := env[engine.EnvUnbudgeted]; reason != "" {
			return r.passThroughUnbudgeted(reason)
		}
		if env[engine.EnvTokenFDs] != "" {
			held, err := engine.ValidateInherited(env)
			if err != nil {
				return r.refuse("the inherited grant does not validate: %v; refusing rather than queueing "+
					"behind a parent that may hold the whole budget (use --independent to queue anyway)", err)
			}
			return r.passThrough(held)
		}
	}
	r.env = dropInherited(env)
	return r.ordinary()
}

// isContext reports whether an environment variable is grant context: CORESLOT_GRANTED_*,
// CORESLOT_HELD_FD, CORESLOT_TOKEN_FDS or CORESLOT_UNBUDGETED. CORESLOT_STATE_DIR is not.
func isContext(key string) bool {
	return strings.HasPrefix(key, engine.EnvGranted) || key == engine.EnvHeldFD ||
		key == engine.EnvTokenFDs || key == engine.EnvUnbudgeted
}

// dropInherited is what a run that does not pass through does with an inherited context
// (PROTOCOL.md section 17): it closes exactly the descriptors CORESLOT_HELD_FD and
// CORESLOT_TOKEN_FDS name, so that its command cannot pass through to a grant that is not its
// parent's, and returns the environment without the context variables. Every other inherited
// descriptor is left alone. Only regular files at descriptor 3 and above are closed: a context
// that names anything else (a standard stream, a pipe, the runtime's own descriptors, which
// take the numbers a launcher closed) is not a grant's.
func dropInherited(env map[string]string) []string {
	var fds []int
	if v := env[engine.EnvHeldFD]; v != "" {
		if fd, err := strconv.Atoi(v); err == nil {
			fds = append(fds, fd)
		}
	}
	if v := env[engine.EnvTokenFDs]; v != "" {
		if list, err := engine.ParseTokenFDs(v); err == nil {
			fds = append(fds, list...)
		}
	}
	for _, fd := range fds {
		var st unix.Stat_t
		if fd >= 3 && unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFREG {
			_ = unix.Close(fd)
		}
	}
	var out []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !isContext(k) {
			out = append(out, kv)
		}
	}
	return out
}

// withGrant is the environment with CORESLOT_GRANTED_<RES> set to grant for each resource it
// names; every other variable, inherited context included, is passed on unchanged.
func withGrant(grant map[string]int) []string {
	set := make(map[string]string, len(grant))
	for name, n := range grant {
		set[engine.EnvGranted+spec.EnvName(name)] = strconv.Itoa(n)
	}
	var out []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); set[k] == "" {
			out = append(out, kv)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(set)) {
		out = append(out, k+"="+set[k])
	}
	return out
}

// nestedGrant is a pass-through's grant (PROTOCOL.md section 18 rule 2): per requested
// resource min(max, held), where held counts the parent's valid token descriptors of it. A
// resource the parent holds none of runs unbudgeted with its min; one whose min is above what
// the parent holds is clamped to all of it, and clamped records the requested min.
func nestedGrant(req spec.Request, held map[string]int) (grant map[string]int, clamped map[string]int, unbudgeted []string) {
	grant = make(map[string]int, len(req.Need))
	clamped = map[string]int{}
	for name, r := range req.Need {
		h := held[name]
		switch {
		case h == 0:
			grant[name] = r.Min
			unbudgeted = append(unbudgeted, name)
		case r.Min > h:
			grant[name] = h
			clamped[name] = r.Min
		default:
			grant[name] = min(r.Max, h)
		}
	}
	slices.Sort(unbudgeted)
	return grant, clamped, unbudgeted
}

// passThrough runs the command on the parent's grant (PROTOCOL.md section 18 rule 2): no
// queue, no section, no lock. The inherited descriptors are passed on as they are, at the same
// numbers, and CORESLOT_GRANTED_<RES> is narrowed for each requested resource.
func (r *run) passThrough(held map[string][]*os.File) int {
	counts := make(map[string]int, len(held))
	for name, files := range held {
		counts[name] = len(files)
	}
	grant, clamped, unbudgeted := nestedGrant(r.o.request(0), counts)
	for _, name := range slices.Sorted(maps.Keys(clamped)) {
		r.notice("%s: requested %d, the parent run holds %d; running with %d", name, clamped[name], grant[name], grant[name])
	}
	if len(unbudgeted) > 0 {
		r.notice("%s: the parent run holds none; running unbudgeted with %s",
			strings.Join(unbudgeted, ", "), grantText(grant, unbudgeted))
	}
	return r.runChild(child{
		env:        withGrant(grant),
		grant:      grant,
		clamped:    slices.Sorted(maps.Keys(clamped)),
		unbudgeted: unbudgeted,
		nested:     true,
		release:    func() { runtime.KeepAlive(held) },
	})
}

// passThroughUnbudgeted runs under a parent that runs unbudgeted (PROTOCOL.md section 18 rule
// 1): with min of each resource, passing CORESLOT_UNBUDGETED on, writing no degraded record.
func (r *run) passThroughUnbudgeted(reason string) int {
	grant := r.o.minGrant()
	r.notice("the parent run is unbudgeted (%s); running unbudgeted with %s", reason,
		grantText(grant, r.o.sortedNeed()))
	return r.runChild(child{
		env:        withGrant(grant),
		grant:      grant,
		unbudgeted: r.o.sortedNeed(),
		nested:     true,
		release:    func() {},
	})
}

// nestedLog is the run log a pass-through appends its line to: only when FORMAT, read outside
// the section, names a format this client implements (PROTOCOL.md section 20).
func nestedLog() *store.Store {
	dir, _, err := config.StateDir()
	if err != nil || !engine.FormatUnderstood(dir) {
		return nil
	}
	st, err := store.Open(dir)
	if err != nil {
		return nil
	}
	return st
}
