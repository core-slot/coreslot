package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"syscall"

	"github.com/Nezhinskiy/coreslot"
	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// Exit statuses of the CLI (PROTOCOL.md section 19).
const (
	exitDegraded      = 3   // status and doctor: degraded mode
	exitUsage         = 64  // a usage error
	exitInternal      = 70  // an internal error before the command could run
	exitRefusal       = 75  // a refusal; --refusal-code changes it
	exitNotExecutable = 126 // the command is not executable
	exitNotFound      = 127 // the command is not found
	exitInterrupted   = 130 // interrupted while waiting (Ctrl-C)
)

// refusalError is a refusal (PROTOCOL.md section 14): nothing runs and the run exits with Code,
// the refusal code of its --refusal-code (75 when Code is 0).
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

// stateError reports a budget that cannot be used, whatever layer found it: the run fails open,
// or refuses under --strict (PROTOCOL.md section 14). The library's and the store's state errors
// are classified the same way.
type stateError struct {
	Reason string
}

func (e *stateError) Error() string { return "state error: " + e.Reason }

// classify maps an error to the CLI's exit status (PROTOCOL.md sections 14 and 19). failOpen is
// true for a state error: the caller runs the command unbudgeted, or turns it into a refusal
// under --strict, and code is then 0. Exec errors of a command that cannot be started give 127
// (not found) or 126 (not executable); anything else that stops the command from running is an
// internal error, 70.
func classify(err error) (code int, failOpen bool) {
	var (
		usage     *spec.UsageError
		refusal   *refusalError
		interrupt *interruptError
		state     *stateError
		libState  *coreslot.StateError
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
	case errors.As(err, &interrupt):
		return 128 + int(interrupt.Sig), false
	case errors.As(err, &state), errors.As(err, &libState), isStoreStateError(err):
		return 0, true
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return exitNotFound, false
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.ENOEXEC), errors.Is(err, syscall.EISDIR):
		return exitNotExecutable, false
	}
	return exitInternal, false
}

// exitStatus is a finished command's status: its exit code, or 128 + the signal that killed it.
func exitStatus(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
