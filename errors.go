package coreslot

import (
	"errors"

	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// Request is what a client asks for: a label, a priority in [-1000, 1000] (higher runs first)
// and, per resource, a Range. An empty Need means cpu 1..1.
type Request = spec.Request

// Range is a request for one resource: start once Min is free, take up to Max.
type Range = spec.Range

// UsageError reports a request that can never be valid.
type UsageError = spec.UsageError

// The data of Snapshot, the data behind `coreslot status --json`.
type (
	Report       = engine.Report
	HolderInfo   = engine.HolderInfo
	TicketInfo   = engine.TicketInfo
	ResourceInfo = engine.ResourceInfo
	SectionInfo  = engine.SectionInfo
	DegradedInfo = engine.DegradedInfo
)

var (
	// ErrCancelled: the request was cancelled, through a cancel request or by its own Cancel or
	// abandoned Wait, before it was admitted.
	ErrCancelled = engine.ErrCancelled
	// ErrRefused: the operation cannot be done, such as adopting into a state directory whose
	// format this client does not implement.
	ErrRefused = engine.ErrRefused
	// ErrAlreadyAdmitted: Cancel came after the admission; cancellation never revokes a grant.
	ErrAlreadyAdmitted = engine.ErrAlreadyAdmitted
	// ErrUnsupported: the platform has no coreslot budget (Windows).
	ErrUnsupported = errors.New("coreslot: unsupported on this platform")
)

// StateError reports a state directory, configuration or token that cannot be used (PROTOCOL.md
// section 14): the caller decides whether to run unbudgeted.
type StateError struct {
	Reason string
}

func (e *StateError) Error() string { return "coreslot state error: " + e.Reason }

// Options configures Open.
type Options struct {
	// StateDir overrides the state directory, for tests: it must be an absolute path, and the
	// configuration is then read from <StateDir>/config.toml. Empty means the default (or the
	// CORESLOT_STATE_DIR override).
	StateDir string
}
