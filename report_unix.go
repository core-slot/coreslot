//go:build unix

package coreslot

import (
	"time"

	"github.com/core-slot/coreslot/internal/engine"
)

// newReport is the engine's snapshot in this package's types: the Unix nanoseconds that
// `status --json` prints as times, and its error texts as the errors behind them.
func newReport(r engine.Report) Report {
	out := Report{
		StateDir:         r.StateDir,
		Format:           r.Format,
		FormatPresent:    r.FormatPresent,
		FormatUnderstood: r.FormatUnderstood,
		Gen:              r.Gen,
		Reconciled:       nsTime(r.ReconciledNS),
		Resources:        make(map[string]ResourceInfo, len(r.Resources)),
		UndeclaredLocked: r.UndeclaredLocked,
		DeadHolds:        r.DeadHolds,
		CancelRequests:   r.Cancels,
		Ignored:          r.Ignored,
		Section: SectionInfo{Held: r.Section.Held, PID: r.Section.PID,
			Entered: nsTime(r.Section.EnteredNS)},
	}
	if r.FormatErr != nil {
		out.FormatError = wrap(r.FormatErr)
	}
	if r.ConfigErr != nil {
		out.ConfigError = wrap(r.ConfigErr)
	}
	for name, ri := range r.Resources {
		out.Resources[name] = ResourceInfo(ri)
	}
	for _, t := range r.Tickets {
		need := make(map[string]Range, len(t.Need))
		for name, rg := range t.Need {
			need[name] = Range(rg)
		}
		out.Tickets = append(out.Tickets, TicketInfo{Name: t.Name, ID: t.ID, Label: t.Label, Priority: t.Priority,
			PID: t.PID, Need: need, Created: nsTime(t.CreatedNS), Refreshed: nsTime(t.MtimeNS), Dead: t.Dead})
	}
	for _, h := range r.Holds {
		out.Holds = append(out.Holds, HoldInfo{Name: h.Name, Label: h.Label, PID: h.PID, Start: nsTime(h.StartNS),
			Granted: h.Grant, FailOpen: h.Unbudgeted, Unbudgeted: h.UnbudgetedResources, Clamped: h.Clamped,
			Adopted: h.Adopted})
	}
	if d := r.Degraded; d != nil {
		out.Degraded = &DegradedInfo{Reason: d.Reason, First: nsTime(d.FirstNS), Count: d.Count,
			Malformed: d.Malformed}
	}
	return out
}

// nsTime is a time the state directory records in Unix nanoseconds, where 0 means none: the zero
// Time. Every other value converts exactly, and back with UnixNano.
func nsTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}
