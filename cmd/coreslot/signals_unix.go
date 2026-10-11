//go:build unix

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// notIgnored drops the signals this process started with ignored: a run must not turn an
// inherited SIG_IGN (an asynchronous command of a shell script ignores SIGINT and SIGQUIT) into
// a caught signal, which its command would then receive with the default disposition.
func notIgnored(sigs ...os.Signal) []os.Signal {
	var out []os.Signal
	for _, s := range sigs {
		if !signal.Ignored(s) {
			out = append(out, s)
		}
	}
	return out
}

// stops emulates the default action of the stop signals SIGTSTP, SIGTTIN and SIGTTOU. The Go
// runtime cannot give a stop signal back its default disposition once it has been caught, so
// the run catches them for its whole life and stops itself with SIGSTOP, except while it holds
// the admission section (inSection), where the stop is deferred until it has left. While a
// command runs in non-terminal mode, a SIGTSTP is first forwarded to the command's own group:
// coreslot then stops as well, so that a shell sees its whole job stopped, and the SIGCONT that
// continues coreslot is forwarded to the command's group.
var stops struct {
	once    sync.Once
	mu      sync.Mutex
	depth   int  // inSection calls in progress
	pending bool // a stop arrived during a section
	child   int  // the command of a non-terminal run, while it runs
}

// installStops starts catching the stop signals; it is idempotent.
func installStops() {
	stops.once.Do(func() {
		sigs := notIgnored(syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU)
		if len(sigs) == 0 {
			return
		}
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, sigs...)
		go func() {
			for s := range ch {
				onStop(s.(syscall.Signal))
			}
		}()
	})
}

func onStop(sig syscall.Signal) {
	// The stop happens under the lock, so no section starts between the decision and the stop.
	stops.mu.Lock()
	defer stops.mu.Unlock()
	switch {
	case stops.child != 0 && sig == syscall.SIGTSTP && stops.depth > 0:
		forward(sig, stops.child, false)
		stops.pending = true
	case stops.child != 0 && sig == syscall.SIGTSTP:
		forward(sig, stops.child, false)
		stopSelf()
	case stops.depth > 0:
		stops.pending = true
	default:
		stopSelf()
	}
}

// inSection runs fn, which may hold the admission section, without letting a stop signal stop
// the process meanwhile: a process stopped inside the section blocks every other client (I5).
// SIGTSTP, SIGTTIN and SIGTTOU arriving during fn are held back, and the process stops once fn
// has returned, as it would have without the section.
func inSection(fn func() error) error {
	installStops()
	stops.mu.Lock()
	stops.depth++
	stops.mu.Unlock()
	err := fn()
	stops.mu.Lock()
	defer stops.mu.Unlock()
	stops.depth--
	if stops.depth == 0 && stops.pending {
		stops.pending = false
		stopSelf()
	}
	return err
}

// prepareChild sets up the command's process group before it starts: in terminal mode it stays
// in coreslot's group; otherwise it gets its own, and SIGCONT is caught from now on so that it
// can be forwarded to that group.
func prepareChild(cmd *exec.Cmd, terminal bool) {
	if terminal {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	signal.Notify(runSignals, notIgnored(syscall.SIGCONT)...)
}

// childDone undoes prepareChild once the command has exited.
func childDone(terminal bool) {
	if !terminal {
		signal.Reset(syscall.SIGCONT)
	}
}

// superviseChild waits for the started command and returns its status (128 + the signal when a
// signal killed it), relaying the signals the run receives meanwhile:
//
//   - terminal mode: the command shares coreslot's group, so SIGINT and SIGQUIT from the
//     terminal reach it directly and coreslot drops its own copies (one addressed to coreslot's
//     pid alone is dropped too: it cannot be told apart); SIGTERM and SIGHUP are forwarded to
//     the command's pid; SIGTSTP stops coreslot as it stops the command, and SIGCONT continues
//     both;
//   - otherwise: SIGINT, SIGQUIT, SIGTERM, SIGHUP, SIGTSTP and SIGCONT are forwarded to the
//     command's own group; after forwarding SIGTSTP coreslot stops too, so that a shell sees its
//     job stopped, and the SIGCONT that continues it is forwarded in turn.
//
// It never signals coreslot's own process group.
func superviseChild(cmd *exec.Cmd, terminal bool) int {
	pid := cmd.Process.Pid
	if !terminal {
		stops.mu.Lock()
		stops.child = pid
		stops.mu.Unlock()
		defer func() {
			stops.mu.Lock()
			stops.child = 0
			stops.mu.Unlock()
		}()
	}
	done := make(chan struct{})
	relayed := make(chan struct{})
	go func() {
		defer close(relayed)
		for {
			select {
			case s := <-runSignals:
				forward(s.(syscall.Signal), pid, terminal)
			case <-done:
				return
			}
		}
	}()
	_ = cmd.Wait()
	close(done)
	<-relayed
	return exitStatus(cmd.ProcessState)
}

// forward relays one signal the run received to its command.
func forward(sig syscall.Signal, pid int, terminal bool) {
	if terminal {
		if sig == syscall.SIGTERM || sig == syscall.SIGHUP {
			_ = unix.Kill(pid, sig)
		}
		return
	}
	if pg, err := unix.Getpgid(pid); err == nil && pg == pid && pg != unix.Getpgrp() {
		_ = unix.Kill(-pid, sig)
		return
	}
	_ = unix.Kill(pid, sig)
}
