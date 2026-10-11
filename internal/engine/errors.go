package engine

import "errors"

// The engine's sentinel errors. They live in a file without a build constraint so that the root
// package can re-export them on every platform.
var (
	// ErrCancelled: the ticket was cancelled (a cancel request was found) or withdrawn by its
	// owner before admission.
	ErrCancelled = errors.New("coreslot: cancelled")
	// ErrRefused: the operation cannot be done here, such as publishing into a state directory
	// whose format this client does not implement.
	ErrRefused = errors.New("coreslot: refused")
	// ErrAlreadyAdmitted: a withdrawal or cancellation came after the admission; it has no
	// effect, because cancellation never revokes a grant.
	ErrAlreadyAdmitted = errors.New("coreslot: already admitted")
	// ErrNoTicket: no ticket with the given id is in the queue (it was admitted, withdrawn or
	// never existed); a cancel request is not published.
	ErrNoTicket = errors.New("coreslot: no such ticket in the queue")
)
