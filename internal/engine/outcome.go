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
)

// Outcome is what the ticket's last admission attempt decided and the numbers it used. Comparable, carried by value.
type Outcome struct {
	Reason                           WaitReason // "" when admitted
	NoPass                           string     // no_typical, headroom_unknown, no_holds, wait_unknown, watch_unreadable, too_long
	W                                int64      // seconds, valid when WKnown
	WKnown                           bool
	CalmLeftS                        float64
	HeadroomMiB, ReserveMiB, RoomMiB int64
	HeadroomOK                       bool
	WatchGate                        string // checked, stale, unknown, no_holds
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
