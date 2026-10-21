//go:build unix

package main

import (
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/store"
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

// decide decides the run's context from its inherited environment (PROTOCOL.md section 18, the
// rule engine.DecideNested applies for the library too): an inherited CORESLOT_UNBUDGETED passes
// through unbudgeted, an inherited CORESLOT_TOKEN_FDS passes through on the parent's grant or
// refuses when it does not validate, and anything else, or --independent, is an ordinary
// request that drops the inherited context first.
//
// Each path that may start a command starts the descriptor-limit probe first, so that the probe
// runs while the request queues. The inherited grant is close-on-exec from the start of the
// process (shieldInherited), so neither the probe nor the getent that deciding may start
// inherits it; an ordinary run starts the probe once that grant is closed and before it has a
// grant of its own.
func (r *run) decide() int {
	env := environ()
	nested, err := engine.DecideNested(env, r.o.independent, "")
	if err != nil {
		return r.refuse("the inherited grant does not validate: %s; refusing rather than queueing "+
			"behind a parent that may hold the whole budget (use --independent to queue anyway)", text(err.Error()))
	}
	switch nested.Kind {
	case engine.NestedUnbudgeted:
		r.nofile = startNofileProbe()
		return r.passThroughUnbudgeted(nested.Reason)
	case engine.NestedPassThrough:
		r.nofile = startNofileProbe()
		return r.passThrough(nested.Held)
	}
	r.env = dropInherited(env)
	r.nofile = startNofileProbe()
	return r.ordinary()
}

// shielded is the inherited context's descriptors that shieldInherited made close-on-exec, with
// their flags from before; a descriptor dropInherited closes leaves it.
var shielded struct {
	mu  sync.Mutex
	fds []shieldedFD
}

// shieldedFD is a descriptor that was made close-on-exec, with its descriptor flags before.
type shieldedFD struct{ fd, flags int }

// shieldInherited makes close-on-exec every descriptor from 3 that the inherited context names
// (CORESLOT_HELD_FD and CORESLOT_TOKEN_FDS) and that lacks the flag. main calls it before any
// command runs, so before this process can start any child: the home directory's NSS fallback
// (getent), which a pass-through's context check and every opener of the default state directory
// may start, the descriptor-limit probe, and anything added later. A child that inherited a
// grant's descriptors would keep its locks for as long as it runs, after every holder of the
// grant has exited (getent for up to its 5 s bound and the wait after it). Only a run's command
// gets them back (exposeInherited); every other command keeps them close-on-exec until it exits.
func shieldInherited() {
	shielded.mu.Lock()
	defer shielded.mu.Unlock()
	shielded.fds = setCloseOnExec(contextFDs(environ()))
}

// forgetInherited drops fd from the shielded descriptors once this process has closed it: the
// next descriptor it opens may take the number, and must stay close-on-exec.
func forgetInherited(fd int) {
	shielded.mu.Lock()
	defer shielded.mu.Unlock()
	shielded.fds = slices.DeleteFunc(shielded.fds, func(s shieldedFD) bool { return s.fd == fd })
}

// exposeInherited calls start, which starts the run's command, with the shielded descriptors
// at their flags from before shieldInherited, so that the command inherits what it would have
// inherited without the shield (a pass-through's grant, or a context descriptor that is not a
// grant's, PROTOCOL.md section 17), and makes them close-on-exec again once start has returned:
// exec.Cmd.Start returns only once the command has exec'd and holds its own copies, so a child
// the run starts after it (the run log's getent) inherits nothing. Nothing else starts a process
// meanwhile: the probe has been collected by then.
func exposeInherited(start func() error) error {
	shielded.mu.Lock()
	defer shielded.mu.Unlock()
	for _, s := range shielded.fds {
		_, _ = unix.FcntlInt(uintptr(s.fd), unix.F_SETFD, s.flags)
	}
	err := start()
	for _, s := range shielded.fds {
		_, _ = unix.FcntlInt(uintptr(s.fd), unix.F_SETFD, s.flags|unix.FD_CLOEXEC)
	}
	return err
}

// setCloseOnExec sets close-on-exec on each open descriptor of fds from 3 that lacks it and
// returns those it changed, with their previous flags.
func setCloseOnExec(fds []int) []shieldedFD {
	var changed []shieldedFD
	for _, fd := range fds {
		if fd < 3 {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC != 0 {
			continue
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err == nil {
			changed = append(changed, shieldedFD{fd, flags})
		}
	}
	return changed
}

// contextFDs is the descriptors CORESLOT_HELD_FD and CORESLOT_TOKEN_FDS name, as far as they
// parse.
func contextFDs(env map[string]string) []int {
	var fds []int
	if v := env[engine.EnvHeldFD]; v != "" {
		if fd, ok := engine.ParseFD(v); ok {
			fds = append(fds, fd)
		}
	}
	if v := env[engine.EnvTokenFDs]; v != "" {
		if list, err := engine.ParseTokenFDs(v); err == nil {
			fds = append(fds, list...)
		}
	}
	return fds
}

// dropInherited is what a run that does not pass through does with an inherited context
// (PROTOCOL.md section 17): it closes exactly the descriptors CORESLOT_HELD_FD and
// CORESLOT_TOKEN_FDS name, so that its command cannot pass through to a grant that is not its
// parent's, and returns the environment without the context variables. Every other inherited
// descriptor is left alone. Only regular files at descriptor 3 and above are closed: a context
// that names anything else (a standard stream, a pipe, the runtime's own descriptors, which
// take the numbers a launcher closed) is not a grant's; it stays shielded until the command
// starts, and the command inherits it.
func dropInherited(env map[string]string) []string {
	for _, fd := range contextFDs(env) {
		var st unix.Stat_t
		if fd >= 3 && unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFREG {
			_ = unix.Close(fd)
			forgetInherited(fd)
		}
	}
	var out []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !engine.IsContext(k) {
			out = append(out, kv)
		}
	}
	return out
}

// withGrant is the environment with CORESLOT_GRANTED_<RES> set to grant for each resource it
// names; every other variable, inherited context included, is passed on unchanged.
func withGrant(grant map[string]int) []string {
	granted := engine.ChildEnv(grant, false, 0, "")
	set := make(map[string]bool, len(granted))
	for _, kv := range granted {
		k, _, _ := strings.Cut(kv, "=")
		set[k] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !set[k] {
			out = append(out, kv)
		}
	}
	return append(out, granted...)
}

// passThrough runs the command on the parent's grant (PROTOCOL.md section 18 rule 2): no
// queue, no section, no lock. The inherited descriptors are passed on as they are, at the same
// numbers, and CORESLOT_GRANTED_<RES> is narrowed for each requested resource.
func (r *run) passThrough(held map[string][]*os.File) int {
	counts := make(map[string]int, len(held))
	for name, files := range held {
		counts[name] = len(files)
	}
	grant, clamped, unbudgeted := engine.NestedGrant(r.o.request(0), counts)
	for _, name := range r.namedOnly(slices.Sorted(maps.Keys(clamped))) {
		n := amount(name, grant[name])
		r.notice("%s: requested %s, the parent run holds %s; running with %s", name, amount(name, clamped[name]), n, n)
	}
	if none := r.namedOnly(unbudgeted); len(none) > 0 {
		r.notice("%s: the parent run holds none; running unbudgeted with %s",
			strings.Join(none, ", "), grantText(grant, none))
	}
	return r.runChild(child{
		env:        withGrant(grant),
		grant:      grant,
		clamped:    slices.Sorted(maps.Keys(clamped)),
		unbudgeted: unbudgeted,
		nested:     true,
		release: func() error {
			runtime.KeepAlive(held)
			return nil
		},
	})
}

// passThroughUnbudgeted runs under a parent that runs unbudgeted (PROTOCOL.md section 18 rule
// 1): with min of each resource, passing CORESLOT_UNBUDGETED on, writing no degraded record.
func (r *run) passThroughUnbudgeted(reason string) int {
	grant := r.o.minGrant()
	r.notice("the parent run is unbudgeted (%s); running unbudgeted with %s", text(reason),
		grantText(grant, r.o.sortedNeed()))
	return r.runChild(child{
		env:        withGrant(grant),
		grant:      grant,
		unbudgeted: r.o.sortedNeed(),
		nested:     true,
		release:    func() error { return nil },
	})
}

// appendNestedLog appends a pass-through's run-log line (PROTOCOL.md section 20): only when
// FORMAT, read outside the section just before the append, names a format this client
// implements, and without opening the state directory as a Store, so nothing but log/ and its
// file is ever created. With no FORMAT (it does not exist: no state directory yet) it writes no
// line and that is not an error; any other FORMAT it does not understand (another format, a
// malformed one, one that cannot be read) is reported.
func appendNestedLog(rec store.RunRecord) error {
	dir, _, err := config.StateDir()
	if err != nil {
		return nil
	}
	if missing, err := formatStillUnderstood(dir); err != nil {
		if missing {
			return nil
		}
		return err
	}
	return store.AppendRunAt(dir, rec)
}
