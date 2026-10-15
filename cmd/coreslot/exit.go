package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"syscall"

	"github.com/core-slot/coreslot"
	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
)

// Exit statuses of the CLI (PROTOCOL.md section 19).
const (
	exitDegraded      = 3   // status and doctor: degraded mode
	exitUsage         = 64  // a usage error
	exitInternal      = 70  // an internal error before the command could run
	exitRefusal       = 75  // a refusal; --refusal-code changes it
	exitNotExecutable = 126 // the command is not executable
	exitNotFound      = 127 // the command is not found
)

// refusalError is a refusal (PROTOCOL.md section 14): nothing runs and the run exits with Code,
// the refusal code of its --refusal-code (75 when Code is 0). It carries text alone on purpose:
// under --strict it replaces a state error, which neither stateReason nor an errors.As for a
// state error may then find in it.
type refusalError struct {
	Msg  string
	Code int
}

func (e *refusalError) Error() string { return e.Msg }

// interruptError reports a run interrupted by a signal before its command started: it exits
// 128 + the signal, which is 130 for SIGINT.
type interruptError struct {
	Sig syscall.Signal
}

func (e *interruptError) Error() string { return "interrupted by " + e.Sig.String() }

// stateError reports a budget that cannot be used, found by the CLI itself (an unusable state
// directory location or configuration): the run fails open, or refuses under --strict
// (PROTOCOL.md section 14).
type stateError struct {
	Reason string
	Err    error // the cause, or nil
}

func (e *stateError) Error() string { return "state error: " + e.Reason }

// Unwrap returns the cause, or nil.
func (e *stateError) Unwrap() error { return e.Err }

// execError is the run's own failure to start its command, typed at the exec: only there does a
// missing or non-executable file mean 127 or 126. The same errno anywhere else is I/O on the state.
type execError struct {
	Err error
}

func (e *execError) Error() string { return e.Err.Error() }

func (e *execError) Unwrap() error { return e.Err }

// classify maps every error to the CLI's exit status (PROTOCOL.md sections 14 and 19). Usage
// errors, refusals (the CLI's own and the engine's ErrRefused), interrupts and the run's exec
// failures have their own statuses. Every other error is judged by stateReason: a state error
// gives failOpen true and code 0 (a run fails open or, under --strict, refuses; status, cancel
// and doctor have their own status for it), and the rest is an internal error, 70.
func classify(err error) (code int, failOpen bool) {
	var (
		usage     *spec.UsageError
		refusal   *refusalError
		interrupt *interruptError
		exe       *execError
	)
	switch {
	case err == nil:
		return 0, false
	case errors.As(err, &usage):
		return exitUsage, false
	case errors.As(err, &refusal):
		if refusal.Code == 0 {
			return exitRefusal, false
		}
		return refusal.Code, false
	case errors.Is(err, engine.ErrRefused):
		return exitRefusal, false
	case errors.As(err, &interrupt):
		return 128 + int(interrupt.Sig), false
	case errors.As(err, &exe):
		switch {
		case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
			return exitNotFound, false
		case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.ENOEXEC), errors.Is(err, syscall.EISDIR):
			return exitNotExecutable, false
		}
		return exitInternal, false
	}
	if _, ok := stateReason(err); ok {
		return 0, true
	}
	return exitInternal, false
}

// stateReason reports whether err is a state error (PROTOCOL.md section 14) and gives its
// reason: the CLI's and the library's own state errors, and on a platform with a budget whatever
// the engine's error contract (engine.StateReason) calls one.
func stateReason(err error) (reason string, ok bool) {
	var (
		state    *stateError
		libState *coreslot.StateError
	)
	switch {
	case errors.As(err, &state):
		return state.Reason, true
	case errors.As(err, &libState):
		return libState.Reason, true
	}
	return engineStateReason(err)
}

// exitStatus is a finished command's status: its exit code, or 128 + the signal that killed it.
func exitStatus(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
