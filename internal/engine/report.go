package engine

// The inspection types live in a file without a build constraint so that the root package can
// alias them on every platform.

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
}

// TicketInfo describes one queue entry in a snapshot.
type TicketInfo struct {
	Name      string // the ticket's file name
	ID        string // its first three fields: what a cancel request names
	Label     string // unescaped: escape it before printing
	Priority  int
	PID       int
	Need      map[string][2]int // resource -> [min, max]
	CreatedNS int64
	MtimeNS   int64 // the last refresh, as a value; never compare it with a clock
	Dead      bool  // its lock probe succeeded: the owner is gone
}

// ResourceInfo is one declared resource in a snapshot.
type ResourceInfo struct {
	Size   int // the current size
	Locked int // tokens locked at every existing index
	Free   int // max(0, Size - Locked)
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

// Report is a snapshot of the state directory: the data behind `status --json`. It is read
// outside the admission section, so it can be momentarily inconsistent.
type Report struct {
	StateDir         string
	Format           int    // 0 when FORMAT is absent
	FormatUnderstood bool   // this client implements Format (an absent FORMAT counts as 1)
	FormatError      string // set when FORMAT is malformed
	Gen              uint64
	ReconciledNS     int64
	Resources        map[string]ResourceInfo // declared resources
	ConfigError      string                  // set when sizes could not be computed
	Tickets          []TicketInfo            // in queue order
	Holds            []HolderInfo            // live holds, in name order
	DeadHolds        []string                // names of holds whose probe succeeded, awaiting reconciliation
	Cancels          []string                // pending cancel requests (ticket ids)
	Ignored          map[string]int          // per directory: entries that do not match the grammar or whose body is malformed
	Section          SectionInfo
	Degraded         *DegradedInfo // nil when degraded mode is not recorded
}
