//go:build unix

package engine

import (
	"context"
	"errors"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
)

// StateReason classifies an error of the engine or the store under the error contract of
// PROTOCOL.md section 14, for the library and the CLI alike. The classes a caller acts on stay
// distinct, and ok is false for them: nil, a *spec.UsageError, the end of a context (canceled
// or past its deadline) and the engine's sentinels (ErrCanceled, ErrRefused, ErrAlreadyAdmitted,
// ErrNoTicket), matched with errors.Is and errors.As. Every other error means the budget cannot
// be used, a state error: ok is true and reason is the *store.StateError's reason when one is
// in err's chain, otherwise err's text (an I/O error, such as a state directory removed under a
// waiter or a directory it may not write).
func StateReason(err error) (reason string, ok bool) {
	var usage *spec.UsageError
	switch {
	case err == nil, errors.As(err, &usage),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, ErrCanceled), errors.Is(err, ErrRefused),
		errors.Is(err, ErrAlreadyAdmitted), errors.Is(err, ErrNoTicket):
		return "", false
	}
	return reasonOf(err), true
}

// reasonOf is err's text without the state error's prefix: a *store.StateError's reason, or
// err's own text.
func reasonOf(err error) string {
	var se *store.StateError
	if errors.As(err, &se) {
		return se.Reason
	}
	return err.Error()
}
