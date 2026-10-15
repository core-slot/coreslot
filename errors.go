package coreslot

import (
	"errors"
	"fmt"

	"github.com/core-slot/coreslot/internal/engine"
)

var (
	// ErrCanceled is returned when the request was canceled before it was admitted: through a
	// cancel request (`coreslot cancel`), or by its own [Waiter.Cancel] or abandoned [Waiter.Wait].
	ErrCanceled = engine.ErrCanceled
	// ErrRefused is returned when the operation cannot be done, such as adopting into a state
	// directory whose format this client does not implement, asking for the free tokens of a
	// resource the configuration does not declare, or passing through an inherited grant that
	// does not validate.
	ErrRefused = engine.ErrRefused
	// ErrAlreadyAdmitted is returned by [Waiter.Cancel] after the admission: cancellation never
	// revokes a grant.
	ErrAlreadyAdmitted = engine.ErrAlreadyAdmitted
	// ErrUnsupported is returned by [Open] on a platform that has no coreslot budget (any but
	// Unix). It matches errors.ErrUnsupported, but test for it with
	// errors.Is(err, coreslot.ErrUnsupported): a [*StateError] can match errors.ErrUnsupported
	// too, when its cause is a filesystem that refuses flock(2).
	ErrUnsupported = fmt.Errorf("coreslot: %w on this platform", errors.ErrUnsupported)
)

// StateError reports a state directory, configuration or token that cannot be used (PROTOCOL.md
// section 14): the caller decides whether to run unbudgeted. Every error of a [Client], [Waiter]
// or [Grant] is one, except nil, a [*UsageError], the end of a context (a [*SectionBusyError]
// included), and [ErrCanceled], [ErrRefused] and [ErrAlreadyAdmitted]. It wraps the error that
// caused it, so errors.Is still finds, for example, fs.ErrPermission or fs.ErrNotExist.
type StateError struct {
	// Reason says what cannot be used, and why.
	Reason string

	err error
}

func (e *StateError) Error() string { return "coreslot: state error: " + e.Reason }

// Unwrap returns the error that caused the state error, or nil.
func (e *StateError) Unwrap() error { return e.err }
