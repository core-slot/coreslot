// Package coreslot is the Go library of coreslot: a cooperative, daemon-less, machine-wide
// budget of CPU cores (and other declared resources) shared by every process of one user
// through a flock(2) file protocol (PROTOCOL.md).
//
//	c, err := coreslot.Open(coreslot.Options{})
//	w, err := c.Enqueue(coreslot.Request{Label: "ci:o-r",
//		Need: map[string]coreslot.Range{"cpu": {Min: 3, Max: 3}}})
//	g, err := w.Wait(ctx)          // or Poll at least every 2 s
//	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
//		// Inherited grant context is replaced by g.Env (PROTOCOL.md section 17).
//		return strings.HasPrefix(kv, "CORESLOT_GRANTED_") ||
//			strings.HasPrefix(kv, "CORESLOT_HELD_FD=") ||
//			strings.HasPrefix(kv, "CORESLOT_TOKEN_FDS=") ||
//			strings.HasPrefix(kv, "CORESLOT_UNBUDGETED=")
//	})
//	cmd.ExtraFiles, cmd.Env = g.Files(), append(env, g.Env()...)
//	... run the command ...
//	g.Close()
//
// The example is the same for an ordinary grant and for a pass-through. Inside another run's
// grant, Enqueue does not queue behind its own parent: it passes through as a nested
// `coreslot run` does (PROTOCOL.md section 18, see Client.EnqueueContext), and the grant's Files
// are the descriptors this process inherited, which Env names at the numbers ExtraFiles gives
// them; Close leaves them open. Only Enqueue and EnqueueContext pass through, and only on a
// Client whose state directory is the one the inherited environment names. An ordinary grant is
// therefore one made outside any grant, where nothing was inherited, unless Options.Independent
// asks for one inside it, the Client is opened with Options.StateDir on another directory, or the
// grant is Adopt's, which never passes through. Then the descriptors the inherited
// CORESLOT_HELD_FD and CORESLOT_TOKEN_FDS name are not the grant's, and section 17 asks a client
// to close them before it starts its command; the library leaves that to the caller, since it
// does not own them and a pass-through of the same process may still need them.
//
// The library never prints, never installs signal handlers, never chooses exit codes and never
// decides on its own to run unbudgeted: a *StateError tells the caller that the budget is
// unusable, and the caller decides. Open locates and opens the state directory before any
// request, so under a parent that runs unbudgeted because that directory is unusable (a relative
// CORESLOT_STATE_DIR, a refused filesystem, wrong owners or modes, a failed home-directory
// lookup), Open returns a *StateError where a nested `coreslot run` would follow the parent; a
// caller that wants to follow it reads CORESLOT_UNBUDGETED. Once Open succeeds, an Enqueue under
// a parent that runs unbudgeted passes through unbudgeted, the parent's decision passed on as the
// CLI passes it. On Windows, Open returns ErrUnsupported and nothing else is reachable. The
// module stays at v0 until format 1 is frozen.
//
// A ticket's lock is carried by a close-on-exec descriptor. If the caller's own goroutines fork
// (os/exec, for example) while the library holds a ticket, the child shares that lock until it
// execs, and an admission or a withdrawal in that window leaves the ticket in queue/, dead once
// the child has exec'd, until the next reconciliation removes it; no live entry is ever removed,
// and the library cannot close this window.
package coreslot
