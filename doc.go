// Package coreslot is the Go library of coreslot: a cooperative, daemon-less, machine-wide
// budget of CPU cores (and other declared resources) shared by every process of one user
// through a flock(2) file protocol (PROTOCOL.md).
//
//	c, err := coreslot.Open(coreslot.Options{})
//	w, err := c.Enqueue(coreslot.Request{Label: "ci:o-r",
//		Need: map[string]coreslot.Range{"cpu": {Min: 3, Max: 3}}})
//	g, err := w.Wait(ctx)          // or Poll at least every 2 s
//	cmd.ExtraFiles, cmd.Env = g.Files(), append(os.Environ(), g.Env()...)
//	... run the command ...
//	g.Close()
//
// The library never prints, never installs signal handlers, never chooses exit codes and never
// decides to run unbudgeted: a *StateError tells the caller that the budget is unusable, and
// the caller decides. On Windows, Open returns ErrUnsupported and nothing else is reachable.
// The module stays at v0 until format 1 is frozen.
package coreslot
