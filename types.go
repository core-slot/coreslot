package coreslot

import (
	"fmt"
	"time"

	"github.com/core-slot/coreslot/internal/spec"
)

// The request and snapshot types live in a file without a build constraint, so that a program
// that names them compiles on every platform.

// Request is what a client asks for: a label, a priority and, per resource, a Range.
type Request struct {
	// Label names the request in `coreslot status` and the run log: valid UTF-8 of at most 128
	// bytes. It may be empty.
	Label string
	// Priority is in [-1000, 1000], higher first; waiting ages a ticket by one point a minute
	// (PROTOCOL.md section 9).
	Priority int
	// Need maps a resource name ([a-z][a-z0-9-]{0,31}) to its Range, for at most 8 resources.
	// An empty Need means cpu 1..1.
	Need map[string]Range
}

// Range is a request for one resource: start once Min is free, take up to Max, with
// 1 <= Min <= Max <= 256. The minimums of one request sum to at most 512.
type Range struct {
	Min, Max int
}

// Validate checks every bound of format 1 (PROTOCOL.md sections 5, 6 and 7) and returns a
// *UsageError for the first one r breaks. It checks r as Enqueue and Adopt would take it, so an
// empty Need is cpu 1..1 and Request{}.Validate() is nil. Enqueue and Adopt validate on their
// own; Validate is for a caller that wants to refuse a request before it opens a budget.
func (r Request) Validate() error {
	if err := spec.Normalize(r.spec()).Validate(); err != nil {
		return &UsageError{Msg: err.Error()}
	}
	return nil
}

// spec is r as the engine takes it.
func (r Request) spec() spec.Request {
	need := make(map[string]spec.Range, len(r.Need))
	for name, rg := range r.Need {
		need[name] = spec.Range(rg)
	}
	return spec.Request{Label: r.Label, Priority: r.Priority, Need: need}
}

// UsageError reports a request that can never be valid, whatever the state of the budget: a
// malformed resource name, a range out of bounds, too many resources, a priority out of range
// or a label that is not valid UTF-8 or too long. Msg says which.
type UsageError struct {
	Msg string
}

func (e *UsageError) Error() string { return "coreslot: " + e.Msg }

// SectionBusyError is the end of a caller's context while another client held the admission
// section (Enqueue, Adopt). PID is the pid that admit.lock's informational body records for that
// client (PROTOCOL.md section 7.7), 0 when it records none; the record may be stale. A process
// stopped inside the section blocks every client until it is continued or killed, and PID is
// where to look. The error wraps the context's error, so errors.Is matches
// context.DeadlineExceeded or context.Canceled.
type SectionBusyError struct {
	PID int
	Err error
}

func (e *SectionBusyError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("coreslot: the admission section is held by pid %d: %v", e.PID, e.Err)
	}
	return fmt.Sprintf("coreslot: the admission section is held: %v", e.Err)
}

// Unwrap returns the context's error.
func (e *SectionBusyError) Unwrap() error { return e.Err }

// Report is a snapshot of the state directory, the data behind `coreslot status --json`, which
// prints the times below as Unix nanoseconds. It is read outside the admission section, so it
// can be momentarily inconsistent.
type Report struct {
	// StateDir is the state directory read.
	StateDir string
	// StateDirExists is false on a machine where nothing has run yet; everything else is then
	// empty.
	StateDirExists bool
	// Format is the format FORMAT names: 0 when FORMAT is absent, unreadable or malformed, and
	// when it names format 0.
	Format int
	// FormatPresent reports that FORMAT was read, well-formed or malformed; it is false when
	// FORMAT is absent or unreadable.
	FormatPresent bool
	// FormatUnderstood reports that this client implements Format; an absent FORMAT counts as
	// format 1, which the first client writes. Under a format it does not implement, a client
	// runs in token-only mode (Grant.TokenOnly).
	FormatUnderstood bool
	// FormatError is a *StateError when FORMAT is malformed or unreadable, and nil otherwise.
	FormatError error
	// Gen is the generation counter, in [0, 2^63-1], bumped when a ticket or a hold is published
	// or removed (PROTOCOL.md section 7.8).
	Gen int64
	// Reconciled is when the last reconciliation ran (it removes dead entries), the zero Time
	// when none has.
	Reconciled time.Time
	// Resources maps each declared resource to its size and use; it is empty when ConfigError
	// is set.
	Resources map[string]ResourceInfo
	// ConfigError is a *StateError when the sizes could not be computed (an invalid
	// configuration, an unknown CPU count), and nil otherwise.
	ConfigError error
	// UndeclaredLocked maps each resource the configuration does not declare, but whose
	// directory under res/ still has locked tokens, to the number locked.
	UndeclaredLocked map[string]int
	// Tickets is the queue, in queue order.
	Tickets []TicketInfo
	// Holds lists the live holds, in name order.
	Holds []HolderInfo
	// DeadHolds names the holds whose owner no longer holds them, awaiting reconciliation.
	DeadHolds []string
	// Cancels lists the pending cancel requests, by ticket id.
	Cancels []string
	// Ignored maps a directory of the layout (queue, holds, cancel) to its entries that do not
	// match the grammar, whose body is malformed, or that fail the filesystem policy: never
	// trusted, only counted.
	Ignored map[string]int
	// Section is the admission section.
	Section SectionInfo
	// Degraded is the degraded-mode record, nil when degraded mode is not recorded.
	Degraded *DegradedInfo
}

// HolderInfo describes one live hold.
type HolderInfo struct {
	Name  string // the hold's file name
	Label string // the holder's label, unescaped: escape it before printing
	PID   int    // the owner's pid, from the name
	// Start is the wall clock at the admission, from the name.
	Start time.Time
	// Grant maps each resource to the number of tokens held.
	Grant map[string]int
	// FailOpen reports a fail-open hold: the run went ahead without the budget, because the
	// budget could not be used, and holds no tokens.
	FailOpen bool
	// Unbudgeted lists, sorted, the requested resources the holder runs without tokens: those
	// the configuration did not declare at the admission.
	Unbudgeted []string
	// Clamped maps each resource whose requested Min exceeded its whole size to that Min; the
	// hold has all of it.
	Clamped map[string]int
	// Adopted reports an adoption (Client.Adopt): work admitted without queueing.
	Adopted bool
}

// TicketInfo describes one queue entry.
type TicketInfo struct {
	Name     string // the ticket's file name
	ID       string // its first three fields: what a cancel request names
	Label    string // unescaped: escape it before printing
	Priority int
	PID      int // the owner's pid, from the name
	Need     map[string]Range
	// Created is the wall clock at the enqueue, from the ticket's body.
	Created time.Time
	// Refreshed is the ticket file's modification time: its last refresh. Compare it only with
	// another Refreshed of the same ticket, never with a clock: an owner refreshes it at least
	// every 2 s, and one whose Refreshed stays the same for 10 s is STALLED.
	Refreshed time.Time
	// Dead reports that the ticket's lock probe succeeded, so nobody holds its descriptor: its
	// owner exited, withdrew, or switched to token-only mode under a format it does not
	// implement, in which case it still runs but writes nothing, not even the removal of this
	// file (PROTOCOL.md section 15.2). Dead says nothing about the owner process.
	Dead bool
}

// ResourceInfo is one declared resource.
type ResourceInfo struct {
	Size   int // the current size
	Locked int // tokens locked at every existing index
	Free   int // max(0, Size - Locked)
}

// SectionInfo is the admission section: whether it is held now, and admit.lock's informational
// body, which may be stale.
type SectionInfo struct {
	// Held reports that a probe of admit.lock failed: some client is inside the section.
	Held bool
	// PID is the pid admit.lock's body records, 0 when the body is unreadable.
	PID int
	// Entered is when that client entered the section, the zero Time when the body is
	// unreadable.
	Entered time.Time
}

// DegradedInfo is the degraded-mode record: runs went ahead without the budget because it could
// not be used (PROTOCOL.md section 19). `coreslot doctor --clear` removes it.
type DegradedInfo struct {
	// Reason is the state error the most recent such run recorded.
	Reason string
	// First is when the first run recorded failed open.
	First time.Time
	// Count is the number of runs that failed open since First.
	Count int64
	// Malformed reports a record whose body could not be read: degraded mode is still recorded,
	// and the other fields are empty.
	Malformed bool
}
