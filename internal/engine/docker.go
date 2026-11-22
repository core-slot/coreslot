//go:build unix

package engine

import (
	"context"
	"time"

	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
)

// A gated ticket is judged by two tests: the Mac test, which is what any
// request is judged by, inside the section and with no Docker call; and the VM test, inside the
// section too but on a reading of Docker's VM that the ticket takes outside it, between two
// entries of the same attempt. A ticket that fits on the host and waits for the VM, or for a
// reading of it, says so with its note, and every other run passes over it.

// ReadEvery is the least time between two Docker readings of a gated ticket, except the one
// immediate re-read of an attempt whose reading the stamp rule rejected.
const ReadEvery = 2 * time.Second

// noteEvery is the least time between two publications of a ticket's note; removals are not
// spaced.
const noteEvery = 2 * time.Second

// failedReadingsUngate is how many failed readings in a row ungate a ticket's Docker part. A count,
// not a clock: a success resets it, and a suspended process misses none.
const failedReadingsUngate = 8

// Why a Docker part goes ungated: Grant.DockerUngated and Outcome.DockerUngated.
const (
	ungatedNoReading = "no-reading" // failedReadingsUngate failed readings in a row
	ungatedOversized = "oversized"  // the Docker need is above V less the Docker reserve
)

// DockerReading is one reading of Docker Desktop's VM, taken outside the admission section.
type DockerReading struct {
	OK         bool  // false: the engine or its VM could not be read, and nothing below is known
	VMMiB      int64 // V, the VM's memory as the engine reports it
	FreeEstMiB int64 // V less what the running containers use
	VMNowMiB   int64 // what the VM's process holds on the host now
	VMM        bool  // the VM runs on Docker VMM, which sets the growth factor (state.Growth)
	StartedNS  int64 // the wall clock (store.NowNS) just before the reading's first request
}

// DockerSource reads Docker Desktop's VM for a gated ticket. Read is called only outside the
// admission section, and never more than once per ReadEvery but for one re-read per attempt.
type DockerSource struct {
	Read func(ctx context.Context) DockerReading
}

// An EnqueueOption changes how a ticket waits.
type EnqueueOption func(*Ticket)

// WithDocker gates a request that has a Docker need on the room in Docker's VM, read through src.
// A request without a Docker need, or without this option, is not gated.
func WithDocker(src DockerSource) EnqueueOption {
	return func(t *Ticket) { t.docker = src }
}

// NoWait is the option of a ticket whose caller makes a single attempt and withdraws it after: a
// VM test that fails publishes no note.
func NoWait() EnqueueOption {
	return func(t *Ticket) { t.noWait = true }
}

// DockerGated reports whether the grant was admitted on its Docker need: its hold reserves the
// VM's growth on the host.
func (g *Grant) DockerGated() bool { return g.dockerGated }

// DockerVMMiB is the VM's memory by the reading that admitted a gated grant; 0 otherwise.
func (g *Grant) DockerVMMiB() int64 { return g.dockerVMMiB }

// DockerUngated says why a request with a Docker need was admitted ungated: "no-reading" after
// failedReadingsUngate failed readings in a row, "oversized" when the need can never fit the VM;
// "" otherwise. An ungated ticket waits as any other from then on, but its ticket keeps its Docker
// need, which the gated tickets behind it still sum.
func (g *Grant) DockerUngated() string { return g.dockerUngated }

// attempt is what one admission attempt of a gated ticket keeps between its section entries. An
// attempt reads at most twice (a reading, and one re-read the stamp rule asks for), so it enters
// the section at most three times.
type attempt struct {
	read   bool // a reading was taken in this attempt
	reread bool // the stamp rule's re-read was taken
}

// gated reports whether the ticket waits for room in Docker's VM: it has a Docker need and a
// source to read the VM, and its Docker part has not gone ungated.
func (t *Ticket) gated() bool {
	return t.docker.Read != nil && t.req.DockerNeedMiB > 0 && t.ungated == ""
}

// gatedTicket reports whether the ticket name ahead, by its body, waits for Docker's VM.
func (t *Ticket) gatedTicket(name string) bool {
	tk := t.tickets[name]
	return tk != nil && tk.DockerNeedMiB > 0
}

// read takes a reading outside the section, and ungates the Docker part after failedReadingsUngate
// failed readings in a row, or at a reading by which the need can never fit the VM.
func (t *Ticket) read(ctx context.Context, at *attempt) {
	t.readAt, at.read = t.e.clock.Monotonic(), true
	r := t.docker.Read(ctx)
	t.reading = r
	if !r.OK {
		if t.failed++; t.failed >= failedReadingsUngate {
			t.ungated = ungatedNoReading
		}
		return
	}
	t.failed = 0
	if t.req.DockerNeedMiB > r.VMMiB-state.DockerReserve(r.VMMiB) {
		t.ungated = ungatedOversized
	}
}

// vmTest is a gated ticket's part of an entry, after its Mac test passed: read reports that the
// ticket must leave the section to take a reading and enter again in the same attempt; admit that
// the VM test passed on the reading in hand. A reading is due when the attempt has taken none and
// the one in hand is ReadEvery old, or once per attempt when the one in hand predates a gated
// hold's sample (the stamp rule). The VM test passes when the Docker need fits the room the reading
// leaves less what the live holds still have to take and what the live gated tickets ahead ask for.
// A failed VM test, or a failed reading, publishes the note.
func (t *Ticket) vmTest(sec *store.Section, holds []liveHold, at *attempt) (read, admit bool, err error) {
	r := t.reading
	switch {
	case !at.read && t.e.clock.Monotonic()-t.readAt >= ReadEvery:
		return true, false, nil
	case !r.OK:
		t.out.Reason = ReasonDockerNoReading
		return false, false, t.note(sec)
	case stamped(r, holds):
		if !at.reread {
			at.reread = true
			return true, false, nil
		}
		t.out.Reason = ReasonDockerNoReading // the next attempt reads again
		return false, false, nil
	}
	reserve := state.DockerReserve(r.VMMiB)
	ahead, err := t.gatedAheadMiB(r.VMMiB - reserve)
	if err != nil {
		return false, false, err
	}
	if t.req.DockerNeedMiB > state.DockerRoom(r.FreeEstMiB, reserve, dockerHolds(holds))-ahead {
		t.out.Reason = ReasonVMRoomShort
		return false, false, t.note(sec)
	}
	return false, true, nil
}

// stamped reports whether r was taken before a live gated hold last sampled its containers: it
// would count that hold's recent growth as free twice.
func stamped(r DockerReading, holds []liveHold) bool {
	for _, h := range holds {
		if h.body.DockerNeedMiB > 0 && h.body.DockerSampledNS > r.StartedNS {
			return true
		}
	}
	return false
}

// dockerHolds are the Docker parts of the live gated holds. A hold whose body did not parse has
// none (holdListing).
func dockerHolds(holds []liveHold) []state.DockerHold {
	var out []state.DockerHold
	for _, h := range holds {
		if b := h.body; b.DockerNeedMiB > 0 {
			var used int64
			if b.DockerFootprintMiB != nil {
				used = *b.DockerFootprintMiB
			}
			out = append(out, state.DockerHold{NeedMiB: b.DockerNeedMiB, FootprintMiB: used})
		}
	}
	return out
}

// gatedAheadMiB is, inside the section, the sum of the Docker needs of the live gated tickets
// ahead of this one, noted or not, leaving out a need above limitMiB: such a ticket can never be
// gated. Live is the re-election's: neither gone, dead, stray nor STALLED for this observer, and
// one never observed counts.
func (t *Ticket) gatedAheadMiB(limitMiB int64) (int64, error) {
	ahead, err := t.listAhead()
	if err != nil {
		return 0, err
	}
	now := t.e.clock.Monotonic()
	var sum int64
	for _, en := range ahead {
		tk, err := t.ticketBody(en.name)
		if err != nil {
			return 0, err
		}
		if tk == nil || tk.DockerNeedMiB == 0 || tk.DockerNeedMiB > limitMiB {
			continue
		}
		live, err := t.liveAhead(en, now)
		if err != nil {
			return 0, err
		}
		if live {
			sum += tk.DockerNeedMiB
		}
	}
	return sum, nil
}

// note publishes the ticket's note inside sec, unless it has one, its caller makes a single
// attempt (NoWait), or it published one less than noteEvery ago.
func (t *Ticket) note(sec *store.Section) error {
	now := t.e.clock.Monotonic()
	if t.noted || t.noWait || now-t.notedAt < noteEvery {
		return nil
	}
	if err := sec.PublishNote(t.Name()); err != nil {
		return err
	}
	t.noted, t.notedAt = true, now
	return nil
}

// unnote removes the ticket's note inside sec, if it has one.
func (t *Ticket) unnote(sec *store.Section) error {
	if !t.noted {
		return nil
	}
	if err := sec.RemoveNote(t.Name()); err != nil {
		return err
	}
	t.noted = false
	return nil
}
