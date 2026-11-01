//go:build unix

package main

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
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

// decide decides the run's context from its inherited environment (PROTOCOL.md section 18):
// unless --independent, an inherited CORESLOT_UNBUDGETED runs unbudgeted under its parent, and a
// CORESLOT_HOLD of a live ancestor's hold runs inside that parent's reservation
// (engine.DecideNested); anything else is an ordinary request that drops the inherited context
// first. A run inside a live parent hold, --independent too, keeps its command in the parent
// command's process group, so that a stop round's kill of that group reaches it; it is no victim.
func (r *run) decide() int {
	env := environ()
	if reason := env[engine.EnvUnbudgeted]; reason != "" && !r.o.independent {
		return r.passThroughUnbudgeted(reason)
	}
	if inside, _ := engine.DecideNested(os.Environ(), isAncestor); inside {
		r.o.sameGroup = true
		if !r.o.independent {
			return r.passThrough(env)
		}
	}
	r.env = dropInherited(env)
	return r.ordinary()
}

// isAncestor reports whether pid is an ancestor of this process. An unreadable process table
// counts as one: the live hold the environment names is then trusted, rather than queueing
// behind a parent that may hold the whole budget.
func isAncestor(pid int) bool {
	procs, ok := memsample.Listing()
	if !ok {
		return true
	}
	for p, seen := os.Getppid(), 0; p > 1 && seen < len(procs); seen++ {
		if p == pid {
			return true
		}
		p = procs[p].PPID
	}
	return false
}

// shielded is the inherited hold's descriptor that shieldInherited made close-on-exec, with its
// flags from before; dropInherited closing it forgets it.
var shielded struct {
	mu  sync.Mutex
	fds []shieldedFD
}

// shieldedFD is a descriptor that was made close-on-exec, with its descriptor flags before.
type shieldedFD struct{ fd, flags int }

// shieldInherited makes the descriptor CORESLOT_HELD_FD names close-on-exec when it is 3 or above
// and lacks the flag. main calls it before any command runs, so before this process can start any
// child, such as the home directory's NSS fallback (getent), which every opener of the default
// state directory may start. A child that inherited the hold's descriptor would keep its lock for
// as long as it runs, after every holder of the hold has exited (getent for up to its 5 s bound).
// Only a run's command gets it back (exposeInherited); every other command keeps it close-on-exec
// until it exits.
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
// inherited without the shield (the hold of the parent it runs inside, or a context descriptor
// that is not a hold's, PROTOCOL.md section 17), and makes them close-on-exec again once start
// has returned: exec.Cmd.Start returns only once the command has exec'd and holds its own copies,
// so a child the run starts after it (the run log's getent) inherits nothing.
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

// contextFDs is the descriptor CORESLOT_HELD_FD names, when it parses.
func contextFDs(env map[string]string) []int {
	if fd, ok := engine.ParseFD(env[engine.EnvHeldFD]); ok {
		return []int{fd}
	}
	return nil
}

// dropInherited is what a run that does not run inside its parent does with an inherited context
// (PROTOCOL.md section 17): it closes exactly the descriptor CORESLOT_HELD_FD names, so that its
// command cannot keep a parent's hold alive, and returns the environment without the context
// variables. Every other inherited descriptor is left alone. Only a regular file at descriptor 3
// or above is closed: a context that names anything else (a standard stream, a pipe, the
// runtime's own descriptors, which take the numbers a launcher closed) is not a hold's; it stays
// shielded until the command starts, and the command inherits it.
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

// withGrant is the environment with CORESLOT_GRANTED_MEM set to grantMiB; every other variable,
// inherited context included, is passed on unchanged.
func withGrant(grantMiB int64) []string {
	var out []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k != engine.EnvGrantedMem {
			out = append(out, kv)
		}
	}
	return append(out, engine.ChildEnv(grantMiB, "", "")...)
}

// passThrough runs the command inside the parent's reservation (PROTOCOL.md section 18): no
// queue, no section, no lock. The inherited hold's descriptor is passed on as it is, and
// CORESLOT_GRANTED_MEM is narrowed to the run's need when the parent's grant is larger.
func (r *run) passThrough(env map[string]string) int {
	grant := r.o.need
	parent, err := strconv.ParseInt(env[engine.EnvGrantedMem], 10, 64)
	if err == nil && parent >= 0 && parent < grant {
		if r.o.mem > 0 { // a need the user did not type, from the default or the needs file, is not announced
			r.notice("mem: requested %s, the parent run holds %s; running with %s",
				spec.FormatMiB(grant), spec.FormatMiB(parent), spec.FormatMiB(parent))
		}
		grant = parent
	}
	return r.runChild(child{env: withGrant(grant), grantMiB: grant, nested: true,
		release: func() error { return nil }})
}

// passThroughUnbudgeted runs under a parent that runs unbudgeted (PROTOCOL.md section 18 rule
// 1): with its need, passing CORESLOT_UNBUDGETED on, writing no degraded record.
func (r *run) passThroughUnbudgeted(reason string) int {
	r.notice("the parent run is unbudgeted (%s); running unbudgeted with mem=%s", text(reason), spec.FormatMiB(r.o.need))
	return r.runChild(child{env: withGrant(r.o.need), grantMiB: r.o.need, nested: true,
		release: func() error { return nil }})
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
