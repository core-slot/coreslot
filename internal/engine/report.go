package engine

import "github.com/core-slot/coreslot/internal/spec"

// HolderInfo describes one live hold as a waiter or a snapshot sees it.
type HolderInfo struct {
	Name                string         // the hold's file name
	Label               string         // the holder's label, unescaped: escape it before printing
	PID                 int            // the owner's pid, from the name
	StartNS             int64          // wall clock at admission, from the name
	Grant               map[string]int // resource -> number of tokens held
	Unbudgeted          bool           // a fail-open hold
	UnbudgetedResources []string
	Clamped             map[string]int // resource -> requested min, for clamped resources
	Adopted             bool
	Tree                bool   // the process tree at PID carries the grant's work; status does not show it
	Preemptible         string // why the run is preemptible, "" when it is not; escape it before printing
	// OwnerExited: the process at PID has exited (or its pid was reused) while the hold's lock is
	// still held, so a descendant that inherited the grant's descriptors holds the tokens. Only
	// Snapshot sets it, and only when the process table can be read.
	OwnerExited bool
}

// TicketInfo describes one queue entry in a snapshot.
type TicketInfo struct {
	Name      string // the ticket's file name
	ID        string // its first three fields: what a cancel request names
	Label     string // unescaped: escape it before printing
	Priority  int
	PID       int
	Need      map[string]spec.Range
	CreatedNS int64
	MtimeNS   int64 // the last refresh, as a value; never compare it with a clock
	// Dead: the ticket's lock probe succeeded, so nobody holds its descriptor: its owner exited,
	// withdrew, or switched to token-only mode under a format it does not implement, in which
	// case it still runs but writes nothing, not even the removal of this file (PROTOCOL.md
	// section 15.2). Dead says nothing about the owner process.
	Dead bool
}

// ResourceInfo is one declared resource in a snapshot.
type ResourceInfo struct {
	Size      int // the current size
	Locked    int // tokens locked at every existing index
	Available int // max(0, Size - Locked); status prints it as free
}

// SectionInfo is the informational body of admit.lock and whether the section is held now.
type SectionInfo struct {
	Held      bool  // a probe of admit.lock failed: some client is inside the section
	PID       int   // from admit.lock's body, 0 when the body is unreadable
	EnteredNS int64 // from admit.lock's body, 0 when the body is unreadable
}

// DegradedInfo is the degraded-mode record; Malformed reports a record whose body could not be
// read (degraded mode is still recorded).
type DegradedInfo struct {
	Reason    string
	FirstNS   int64
	Count     int64
	Malformed bool
}

// Report is a snapshot of the state directory: the data behind `status --json`, in its units
// (times in Unix nanoseconds; each error both as the text the JSON carries and as the error, for
// the library). It is read outside the admission section, so it can be momentarily
// inconsistent.
type Report struct {
	StateDir         string
	StateDirExists   bool   // false on a machine where nothing has run yet: everything else is empty
	Format           int    // 0 when FORMAT is absent, unreadable or malformed, and when it names format 0
	FormatPresent    bool   // FORMAT was read, well-formed or malformed; false when it is absent or unreadable
	FormatUnderstood bool   // this client implements Format (an absent FORMAT counts as 1)
	FormatError      string // set when FORMAT is malformed or unreadable
	FormatErr        error  // the error FormatError is the text of
	Gen              int64  // in [0, 2^63-1] (PROTOCOL.md section 7.8)
	ReconciledNS     int64
	Resources        map[string]ResourceInfo // declared resources
	ConfigError      string                  // set when sizes could not be computed
	ConfigErr        error                   // the error ConfigError is the reason of
	UndeclaredLocked map[string]int          // res/<name> directories the configuration does not declare, with locked tokens
	Tickets          []TicketInfo            // in queue order
	Holds            []HolderInfo            // live holds, in name order
	DeadHolds        []string                // names of holds whose probe succeeded, awaiting reconciliation
	Cancels          []string                // pending cancel requests (ticket ids)
	Ignored          map[string]int          // per directory: entries that do not match the grammar or whose body is malformed
	Section          SectionInfo
	Degraded         *DegradedInfo // nil when degraded mode is not recorded
}
