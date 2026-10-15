package engine

import (
	"errors"
	"fmt"
)

// The engine's sentinel errors. They live in a file without a build constraint so that the root
// package can re-export them on every platform.
var (
	// ErrCanceled is returned when the ticket was canceled (a cancel request was found) or
	// withdrawn by its owner before admission.
	ErrCanceled = errors.New("coreslot: canceled")
	// ErrRefused is returned when the operation cannot be done here, such as publishing into a
	// state directory whose format this client does not implement.
	ErrRefused = errors.New("coreslot: refused")
	// ErrAlreadyAdmitted is returned when a withdrawal or cancellation came after the admission;
	// it has no effect, because cancellation never revokes a grant.
	ErrAlreadyAdmitted = errors.New("coreslot: already admitted")
	// ErrNoTicket is returned when no ticket with the given id is in the queue (it was admitted,
	// withdrawn or never existed); a cancel request is not published.
	ErrNoTicket = errors.New("coreslot: no such ticket in the queue")
)

// SectionBusyError is the end of a caller's context while another client held the admission
// section. PID is the pid admit.lock's informational body records (PROTOCOL.md section 7.7), 0
// when it records none: a process stopped inside the section blocks every client until it is
// continued or killed. It matches the context's error (errors.Is with context.DeadlineExceeded
// or context.Canceled).
type SectionBusyError struct {
	PID int
	Err error
}

func (e *SectionBusyError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("the admission section is held by pid %d: %v", e.PID, e.Err)
	}
	return fmt.Sprintf("the admission section is held: %v", e.Err)
}

func (e *SectionBusyError) Unwrap() error { return e.Err }

// IdentityError is Grant.Close's report of a release identity mismatch (I1, PROTOCOL.md section
// 12.3): a hold or token path no longer names the file the grant holds. Err is the mismatch, a
// state error; the holder has written nothing into the state directory, and a caller that writes
// at release (such as the CLI's run log) must not either. Every other error of Close is a failure
// to close a descriptor.
type IdentityError struct {
	Err error
}

func (e *IdentityError) Error() string { return e.Err.Error() }

func (e *IdentityError) Unwrap() error { return e.Err }
