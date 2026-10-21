// Package coreslot is the Go library of coreslot: a cooperative, daemon-less, machine-wide
// budget of CPU cores (and other declared resources) shared by every process of one user
// through a flock(2) file protocol, defined in [PROTOCOL.md].
//
//	c, err := coreslot.Open(coreslot.Options{})
//	if err != nil {
//		return err // a *StateError or ErrUnsupported: the caller decides whether to run unbudgeted
//	}
//	defer c.Close()
//	w, err := c.Enqueue(ctx, coreslot.Request{Label: "make test",
//		Need: map[string]coreslot.Range{"cpu": {Min: 2, Max: 8}}})
//	if err != nil {
//		return err
//	}
//	g, err := w.Wait(ctx) // or Poll at least every 2 s
//	if err != nil {
//		return err
//	}
//	defer g.Close()
//	cmd := exec.CommandContext(ctx, "make", "test", "-j", strconv.Itoa(g.Granted()["cpu"]))
//	cmd.ExtraFiles, cmd.Env = g.Files(), g.Environ(os.Environ())
//	return cmd.Run()
//
// [Open] opens the budget as a [Client]. [Client.Enqueue] queues a [Request] and returns a
// [Waiter], whose [Waiter.Wait] or [Waiter.Poll] returns the [Grant] once it is admitted;
// [Grant.Files] and [Grant.Environ] start a command inside the grant. [Client.Adopt] admits work
// that is already running, without queueing, and [Client.Snapshot] reads the state directory as
// `coreslot status` does.
//
// A request names memory as the resource "mem", in tokens of 128 MiB ({Min: 24, Max: 24} is
// 3 GiB); [Grant.Granted] counts it in tokens, and [Grant.Environ] gives the command
// CORESLOT_GRANTED_MEM in MiB. The library writes no run log, so `coreslot stats` and
// `coreslot suggest` learn nothing from its grants.
//
// # Pass-through
//
// The example is the same for an ordinary grant and for a pass-through. Inside another run's
// grant, Enqueue does not queue behind its own parent: it passes through as a nested
// `coreslot run` does ([PROTOCOL.md] section 18, see [Client.Enqueue]), and the grant's Files
// are the descriptors this process inherited, which Env names at the numbers ExtraFiles gives
// them; Close leaves them open. Only Enqueue passes through, and only on a Client whose state
// directory is the one the inherited environment names. An ordinary grant is therefore one made
// outside any grant, where nothing was inherited, unless [Options].Independent asks for one inside
// it, the Client is opened with [Options].StateDir on another directory, or the grant is Adopt's,
// which never passes through. Then the descriptors the inherited CORESLOT_HELD_FD and
// CORESLOT_TOKEN_FDS name are not the grant's, and section 17 asks a client to close them before
// it starts its command; the library leaves that to the caller, since it does not own them and a
// pass-through of the same process may still need them. [Grant.Environ] drops their names from
// the command's environment either way.
//
// # Errors
//
// The library never prints, never installs signal handlers, never chooses exit codes and never
// decides on its own to run unbudgeted. An error is one of:
//
//   - a [*StateError]: the budget is unusable (its state directory, configuration or a token),
//     and the caller decides whether to run unbudgeted;
//   - [ErrUnsupported]: this platform has no budget, which a caller that runs unbudgeted on a
//     *StateError treats the same way;
//   - a [*UsageError]: the request or the [Options] can never be valid;
//   - the context's error, as a [*SectionBusyError] when another client held the admission
//     section meanwhile;
//   - [ErrCanceled], [ErrRefused] or [ErrAlreadyAdmitted].
//
// Test for the platform with errors.Is(err, coreslot.ErrUnsupported), not with
// errors.ErrUnsupported: a *StateError wraps its cause, and a filesystem that refuses flock(2)
// gives a cause that matches errors.ErrUnsupported too.
//
// Open locates and opens the state directory before any request, so under a parent that runs
// unbudgeted because that directory is unusable (a relative CORESLOT_STATE_DIR, a refused
// filesystem, wrong owners or modes, a failed home-directory lookup), Open returns a *StateError
// where a nested `coreslot run` would follow the parent; a caller that wants to follow it reads
// CORESLOT_UNBUDGETED. Once Open succeeds, an Enqueue under a parent that runs unbudgeted passes
// through unbudgeted, the parent's decision passed on as the CLI passes it.
//
// # Platforms
//
// The budget exists on Unix. macOS and Linux are supported and tested; the other Unix systems
// build but are not tested, except AIX, where the package does not build and which is not
// supported. On a platform without Unix (Windows among them), Open returns ErrUnsupported and
// nothing else is reachable. The module stays at v0 until v1.0.0, which freezes the on-disk
// format then current.
//
// # Forking while a ticket is held
//
// A ticket's lock is carried by a close-on-exec descriptor. If the caller's own goroutines fork
// (os/exec, for example) while the library holds a ticket, the child shares that lock until it
// execs, and an admission or a withdrawal in that window leaves the ticket in queue/, dead once
// the child has exec'd, until the next reconciliation removes it; no live entry is ever removed,
// and the library cannot close this window.
//
// # Home-directory lookup
//
// Without [Options].StateDir or CORESLOT_STATE_DIR, the state directory is under the user's home
// directory, which Open looks up in the password database and, when that finds no record, by
// running getent as a short-lived child process ([Open] says when, and what that asks of the
// caller). Naming the directory skips the lookup entirely.
//
// [PROTOCOL.md]: https://github.com/core-slot/coreslot/blob/main/PROTOCOL.md
package coreslot
