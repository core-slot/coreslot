package engine

import "github.com/core-slot/coreslot/internal/watch"

// HolderInfo describes one live hold as a waiter or a snapshot sees it.
type HolderInfo struct {
	Name         string // the hold's file name
	Label        string // the holder's label, unescaped: escape it before printing
	PID          int    // the owner's pid
	StartNS      int64  // wall clock at admission
	ArrivalNS    int64  // wall clock at the run's first arrival in the queue
	NeedMiB      int64  // what the hold reserves; 0 for a run outside the budget
	FootprintMiB int64  // what its run last measured
	Stops        int
	// OwnerExited: the process at PID has exited (or its pid was reused) while the hold's lock is
	// still held, so a descendant that inherited the hold's descriptor holds the reservation. Only
	// Snapshot sets it, and only when the process table can be read.
	OwnerExited bool
}

// WatchInfo is the shared watch state in a snapshot.
type WatchInfo struct {
	Level      watch.Level
	Excess     float64 // E, in points, at the newest sample
	SampledNS  int64   // when, by the wall clock, the newest sample was taken; 0 when none was
	Fresh      bool    // the newest sample is young enough for admission to read
	Observe    bool    // [watch] mode is observe: nothing is stopped
	NextVictim string  // the hold the next stop round lists first; "" when none
	Learning   int     // running jobs whose label has no normal share yet: no part of E, stopped by a floor round only
}

// TicketInfo describes one queue entry in a snapshot.
type TicketInfo struct {
	Name      string // the ticket's file name
	ID        string // its first three fields: what a cancel request names
	Label     string // unescaped: escape it before printing
	PID       int
	NeedMiB   int64
	CreatedNS int64
	MtimeNS   int64 // the last refresh, as a value; never compare it with a clock
	// Dead: the ticket's lock probe succeeded, so nobody holds its descriptor: its owner exited,
	// withdrew. Dead says nothing about the owner process.
	Dead bool
}

// MemInfo is the memory in a snapshot, in MiB. Known is false when the headroom could not be read
// or the configuration is invalid, and the sizes are then 0.
type MemInfo struct {
	Known       bool
	ReservedMiB int64 // what the live holds reserve
	FreeMiB     int64 // the room a new job may take, never below 0
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
// its callers). It is read outside the admission section, so it can be momentarily
// inconsistent.
type Report struct {
	StateDir         string
	StateDirExists   bool   // false on a machine where nothing has run yet: everything else is empty
	Format           int    // 0 when FORMAT is absent, unreadable or malformed, and when it names format 0
	FormatPresent    bool   // FORMAT was read, well-formed or malformed; false when it is absent or unreadable
	FormatUnderstood bool   // this client implements Format; an absent FORMAT counts as understood
	FormatError      string // set when FORMAT is malformed or unreadable
	FormatErr        error  // the error FormatError is the text of
	Gen              int64  // in [0, 2^63-1] (PROTOCOL.md section 7.8)
	ReconciledNS     int64
	Mem              MemInfo
	ConfigError      string         // set when the memory could not be read
	ConfigErr        error          // the error ConfigError is the reason of
	Tickets          []TicketInfo   // in queue order
	Holds            []HolderInfo   // live holds, in name order
	DeadHolds        []string       // names of holds whose probe succeeded, awaiting reconciliation
	Cancels          []string       // pending cancel requests (ticket ids)
	Ignored          map[string]int // per directory: entries that do not match the grammar or whose body is malformed
	Section          SectionInfo
	Watch            WatchInfo
	Degraded         *DegradedInfo // nil when degraded mode is not recorded
}
