//go:build unix

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/store"
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
// the run catches them for its whole life and stops itself with SIGSTOP, except while it holds a
// lock it took only momentarily (momentaryLockTaken): the admission section, or the probe lock of
// a ticket or hold. There the stop is deferred until it has let go. While a
// command runs in non-terminal mode, a SIGTSTP is first forwarded to the command's own group:
// coreslot then stops as well, so that a shell sees its whole job stopped, and the SIGCONT that
// continues coreslot is forwarded to the command's group.
var stops struct {
	once    sync.Once
	mu      sync.Mutex
	depth   int  // momentary locks in progress, as the store's lock hooks count them
	pending bool // a stop arrived while one was held
	child   int  // the command of a non-terminal run, while it runs
}

// installLockHooks makes the store tell this process when it may hold a lock it takes only
// momentarily, so that a stop is deferred exactly then (PROTOCOL.md section 8): every try to take
// admit.lock and every moment it is held, and every probe of a ticket or hold, whichever
// command and call site makes it, but never the waits between tries to enter the section, during
// which Ctrl-Z stops the process at once. The locks a run owns for a lifetime (its ticket while
// it waits, its grant while its command runs) do not defer a stop: a stopped run keeps only what
// is its own. main calls it before any command runs.
func installLockHooks() { store.SetLockHooks(momentaryLockTaken, momentaryLockLeft) }

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

// momentaryLockTaken is the store's lock hook that runs just before each try to take a momentary
// lock: from now until the matching momentaryLockLeft, a stop signal must not stop the process,
// since a process stopped inside the section blocks every other client, and one stopped holding a
// probe lock makes every other client count that ticket or hold as live (I5). SIGTSTP, SIGTTIN
// and SIGTTOU arriving meanwhile are held back. Holds nest (a probe inside the section) and may
// overlap across goroutines, so they are counted. It runs once per probe, so it does no more than
// take a mutex.
func momentaryLockTaken() {
	installStops()
	stops.mu.Lock()
	stops.depth++
	stops.mu.Unlock()
}

// momentaryLockLeft is the store's lock hook that runs once a momentary lock is no longer held
// (or was not taken): when no other is in progress, a stop held back meanwhile stops the process
// now, as it would have without the lock.
func momentaryLockLeft() {
	stops.mu.Lock()
	defer stops.mu.Unlock()
	stops.depth--
	if stops.depth == 0 && stops.pending {
		stops.pending = false
		stopSelf()
	}
}

// prepareChild sets up the command's process group before it starts: in terminal mode, or with
// --same-group, it stays in coreslot's group; otherwise it gets its own, and SIGCONT is caught
// from now on so that it can be forwarded to that group.
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

// killGroup sends SIGKILL to the process group pgid; tests replace it.
var killGroup = func(pgid int) error { return unix.Kill(-pgid, unix.SIGKILL) }

// supervisor reaps the command and is the one place that kills its process group: the kill and
// the reap take its lock, so the group is signalled only while the command is unreaped and its
// pid, the group's id, cannot have been reused.
type supervisor struct {
	mu     sync.Mutex
	pid    int
	group  bool // the command leads a process group of its own
	reaped bool
	waited bool // the reap read the command's status and rusage
	ws     syscall.WaitStatus
	ru     syscall.Rusage
}

func newSupervisor(cmd *exec.Cmd, group bool) *supervisor {
	return &supervisor{pid: cmd.Process.Pid, group: group}
}

// KillGroup sends SIGKILL to the command's own process group and reports whether it did: never
// once the command is reaped, nor to a group the command shares with coreslot.
func (s *supervisor) KillGroup() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.group && !s.reaped && killGroup(s.pid) == nil
}

// reap waits for the command to exit, woken by SIGCHLD, and reaps it under the lock.
func (s *supervisor) reap() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGCHLD)
	defer signal.Stop(ch)
	for {
		s.mu.Lock()
		pid, err := syscall.Wait4(s.pid, &s.ws, syscall.WNOHANG, &s.ru)
		s.waited = pid == s.pid
		s.reaped = s.waited || err != nil && err != syscall.EINTR
		reaped := s.reaped
		s.mu.Unlock()
		if reaped {
			return
		}
		if err == nil {
			<-ch
		}
	}
}

// usage is the reaped command's rusage; nil when the reap read none.
func (s *supervisor) usage() *syscall.Rusage {
	if !s.waited {
		return nil
	}
	return &s.ru
}

// superviseChild waits for the started command, reaping it through s, and returns its status
// (128 + the signal when a signal killed it), relaying the signals the run receives meanwhile:
//
//   - terminal mode, or --same-group: the command shares coreslot's group, so SIGINT and
//     SIGQUIT from the terminal reach it directly and coreslot drops its own copies (one
//     addressed to coreslot's pid alone is dropped too: it cannot be told apart); SIGTERM and
//     SIGHUP are forwarded to the command's pid; SIGTSTP stops coreslot as it stops the
//     command, and SIGCONT continues both. With --same-group no terminal sends those keys:
//     the group is the caller's to signal (a killpg, SIGKILL included, reaches the whole tree);
//   - otherwise: SIGINT, SIGQUIT, SIGTERM, SIGHUP, SIGTSTP and SIGCONT are forwarded to the
//     command's own group; after forwarding SIGTSTP coreslot stops too, so that a shell sees its
//     job stopped, and the SIGCONT that continues it is forwarded in turn.
//
// It never signals coreslot's own process group.
func superviseChild(cmd *exec.Cmd, s *supervisor, terminal bool) int {
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
			case sig := <-runSignals:
				forward(sig.(syscall.Signal), pid, terminal)
			case <-done:
				return
			}
		}
	}()
	s.reap()
	_ = cmd.Wait() // ends the copying of its I/O; the command is reaped already
	close(done)
	<-relayed
	if s.ws.Signaled() {
		return 128 + int(s.ws.Signal())
	}
	return s.ws.ExitStatus()
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
