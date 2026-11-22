package engine

import (
	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/watch"
)

// HolderInfo describes one live hold as a waiter or a snapshot sees it.
type HolderInfo struct {
	Name         string // the hold's file name
	ID           string // its first three fields: what a request to trim names
	Label        string // the holder's label, unescaped: escape it before printing
	PID          int    // the owner's pid
	StartNS      int64  // wall clock at admission
	ArrivalNS    int64  // wall clock at the run's first arrival in the queue
	NeedMiB      int64  // what the hold reserves; 0 for a run outside the budget
	AskedMiB     int64  // the need it was admitted with, when its body says; 0 when NeedMiB is it
	Trimmed      bool   // its run acted on a request to trim it
	Forced       bool   // admitted at once on a start request, whatever the room
	FootprintMiB int64  // what its run last measured
	Stops        int
	Container    bool  // container work: its containers' memory is not measured unless DockerFootprintMiB is set
	NoCPUS       int64 // how long its tree has used next to no CPU, in s; 0 under the mark
	// DockerFootprintMiB is what its run's own containers have grown by, 0 included; nil when its
	// run does not count them. DockerNote is why its Docker work has no forwarder or is not
	// counted, "" when nothing is to be said.
	DockerFootprintMiB *int64
	DockerNote         string
	// DockerNeedMiB is the Docker need of a run gated on Docker's VM, 0 for any other; DockerGrowthMiB
	// the VM growth its hold reserves on the host.
	DockerNeedMiB, DockerGrowthMiB int64
	// OwnerExited: the process at PID has exited (or its pid was reused) while the hold's lock is
	// still held, so a descendant that inherited the hold's descriptor holds the reservation. Only
	// Snapshot sets it, and only when the process table can be read.
	OwnerExited bool
	ChildExited bool  // its command has exited: its run is releasing it
	SampleAgeMS int64 // since its run last wrote a sample; -1 when it has not
	TypicalS    int64 // its label's typical run time in s; 0 unknown
	// LeftS is what remains of TypicalS at the snapshot's clock, never below 0; meaningful when
	// TypicalS > 0. Only Snapshot sets it.
	LeftS int64
	// Stoppable: the hold's body says victim, its command has not exited
	// and the process table, read by Snapshot, shows its owner alive. False whenever that cannot be
	// told, including an unreadable process table. A convenience for a reader, not a control.
	Stoppable bool
}

// WatchInfo is the shared watch state in a snapshot.
type WatchInfo struct {
	Level       watch.Level
	Excess      float64 // E, in points, at the newest sample
	SampledNS   int64   // when, by the wall clock, the newest sample was taken; 0 when none was
	Fresh       bool    // the newest sample is young enough for admission to read
	Observe     bool    // the watchdog's mode is observe: nothing is stopped
	ModeSource  string  // "config" or "override" (coreslot watch, this machine)
	ModeUntilNS int64   // when an override ends; 0 for until removed, or none
	// EnforceRoundLive: the last round was written in enforce mode and a hold it listed is live,
	// so it still stops that hold whatever the mode is now (PROTOCOL.md section 23.5).
	EnforceRoundLive bool
	NextVictim       string // the hold the next stop round lists first; "" when none
	Learning         int    // running jobs whose label has no normal share yet: no part of E, stopped by a floor round only
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
	// WaitS is the expected wait of PROTOCOL.md section 10.3 in s, valid when WaitKnown. Only
	// Snapshot sets them, and only on the first live ticket in queue order.
	WaitS     int64
	WaitKnown bool
	// DockerNeedMiB is the Docker need of a gated ticket, 0 for any other; Noted that it has its
	// note in vmwait/: it fits on the host and waits for Docker's VM.
	DockerNeedMiB int64
	Noted         bool
}

// MemInfo is the memory in a snapshot, in MiB. Known is false when the headroom could not be read
// or the configuration is invalid, and the sizes are then 0.
type MemInfo struct {
	Known       bool
	ReservedMiB int64 // what the live holds reserve
	FreeMiB     int64 // the room a new job may take, never below 0
	RealFreeMiB int64 // the headroom less the reserve, never below 0: a container request's room
	// What the two rooms came from; 0 and false when the configuration or the machine's memory
	// could not be read.
	HeadroomMiB int64
	HeadroomOK  bool
	ReserveMiB  int64
	MachineMiB  int64 // the machine's memory; 0 unless Known
	RoomMiB     int64 // the room a new job may take before FreeMiB clamps it at 0: negative when over-reserved
}

// StartInfo is one pending start request.
type StartInfo struct {
	Ticket      string // the ticket id
	RequestedNS int64  // wall clock when it was written; 0 when its body is malformed
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
	Config           config.Config  // the configuration this snapshot read; zero when it could not be read
	Tickets          []TicketInfo   // in queue order
	Holds            []HolderInfo   // live holds, in name order
	DeadHolds        []string       // names of holds whose probe succeeded, awaiting reconciliation
	Cancels          []string       // pending cancel requests (ticket ids)
	Starts           []StartInfo    // pending start requests
	Ignored          map[string]int // per directory: entries that do not match the grammar or whose body is malformed
	Section          SectionInfo
	Watch            WatchInfo
	Degraded         *DegradedInfo // nil when degraded mode is not recorded
}
