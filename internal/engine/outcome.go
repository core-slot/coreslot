//go:build unix

package engine

import (
	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/watch"
)

// WaitReason names why a ticket's last admission attempt did not admit it.
type WaitReason string

const (
	ReasonBehindTicket, ReasonSectionBusy, ReasonRoomShort, ReasonBehindHead, ReasonWatchLevel, ReasonWatchSpacing,
	ReasonRestartStale, ReasonRestartLevel, ReasonRestartCalm, ReasonRestartMargin WaitReason = "behind_ticket", "section_busy", "room_short", "behind_head", "watch_level", "watch_spacing",
		"restart_stale", "restart_level", "restart_calm", "restart_margin"
	// A gated ticket that fits on the host waits for room in Docker's VM, or for a reading of it.
	ReasonVMRoomShort, ReasonDockerNoReading WaitReason = "vm_room_short", "docker_no_reading"
	ReasonYielding                           WaitReason = "yielding" // a long head that fits lets short jobs go first
	ReasonQuiet                              WaitReason = "quiet"    // a quiet job runs: no other job is admitted
)

// Outcome is what the ticket's last admission attempt decided and the numbers it used. Comparable, carried by value.
type Outcome struct {
	Reason                           WaitReason // "" when admitted
	NoPass                           string     // no_typical, headroom_unknown, no_holds, wait_unknown, watch_unreadable, too_long, head_fits, out_of_turn_live, hold_unreadable
	Pass                             string     // how an admission ahead of the head was allowed: duration or turn; "" for the head
	W                                int64      // seconds, valid when WKnown
	WKnown                           bool
	CalmLeftS                        float64
	HeadroomMiB, ReserveMiB, RoomMiB int64
	HeadroomOK                       bool
	WatchGate                        string // checked, stale, unknown, no_holds
	Noted                            bool   // the ticket has its note: it fits on the host and waits for Docker's VM
	DockerUngated                    string // why a request with a Docker need is not gated (Grant.DockerUngated)
	YieldUntilNS                     int64  // a yielding head's deadline
	YieldEnded                       string // why the head's yield ended: record, deadline, clock, overrun, headship, no_taker
}

// Admission is the outcome of the attempt that admitted the grant; zero for a grant no attempt made.
func (g *Grant) Admission() Outcome { return g.admission }

// watchGate says what the watch state could tell an attempt with holds live.
func watchGate(ok bool, level watch.Level) string {
	switch {
	case !ok:
		return "stale"
	case level == watch.Unknown:
		return "unknown"
	}
	return "checked"
}

// restartRefusal is the first condition that keeps a restart out, "" when none does, with the calm
// it still needs in ns for restart_calm. Its order is the decision's: only the first true clause runs.
func restartRefusal(ok bool, d watch.Detector, s watch.Sample, at, now int64, c watch.Config, m config.Machine, need, room int64) (WaitReason, int64) {
	if !ok {
		return ReasonRestartStale, 0
	}
	if d.Level != watch.Green {
		return ReasonRestartLevel, 0
	}
	if left := calmLeft(d, s, at, now, c); left > 0 {
		return ReasonRestartCalm, left
	}
	if m.HeadroomOK && room < need+max(256, need/10) {
		return ReasonRestartMargin, 0
	}
	return "", 0
}
