package coreslot

import (
	"errors"
	"fmt"

	"github.com/core-slot/coreslot/internal/engine"
)

var (
	// ErrCanceled is returned when the request was canceled before it was admitted: through a
	// cancel request (`coreslot cancel`), or by its own Waiter's Cancel or abandoned Wait.
	ErrCanceled = engine.ErrCanceled
	// ErrRefused is returned when the operation cannot be done, such as adopting into a state
	// directory whose format this client does not implement, asking for the free tokens of a
	// resource the configuration does not declare, or passing through an inherited grant that
	// does not validate.
	ErrRefused = engine.ErrRefused
	// ErrAlreadyAdmitted is returned by Waiter.Cancel after the admission: cancellation never
	// revokes a grant.
	ErrAlreadyAdmitted = engine.ErrAlreadyAdmitted
	// ErrUnsupported is returned by Open on a platform that has no coreslot budget (any but
	// Unix). It matches errors.ErrUnsupported.
	ErrUnsupported = fmt.Errorf("coreslot: %w on this platform", errors.ErrUnsupported)
)

// StateError reports a state directory, configuration or token that cannot be used (PROTOCOL.md
// section 14): the caller decides whether to run unbudgeted. Every error of a Client, Waiter or
// Grant is one, except nil, a *UsageError, the end of a context (a *SectionBusyError included),
// and ErrCanceled, ErrRefused and ErrAlreadyAdmitted. It wraps the error that caused it, so
// errors.Is still finds, for example, fs.ErrPermission or fs.ErrNotExist.
type StateError struct {
	// Reason says what cannot be used, and why.
	Reason string

	err error
}

func (e *StateError) Error() string { return "coreslot: state error: " + e.Reason }

// Unwrap returns the error that caused the state error, or nil.
func (e *StateError) Unwrap() error { return e.err }

// Options configures Open.
type Options struct {
	// StateDir overrides the state directory, for tests: it must be an absolute path, and the
	// configuration is then read from <StateDir>/config.toml. Empty means the default (or the
	// CORESLOT_STATE_DIR override).
	StateDir string
	// Independent makes Enqueue an ordinary request even inside another run's grant, as the
	// CLI's --independent does: it queues in the budget like an unrelated request, behind a
	// parent that may hold the whole budget, instead of passing through (see Client.Enqueue).
	Independent bool
}
