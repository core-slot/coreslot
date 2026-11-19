//go:build unix

package engine

import (
	"context"
	"os"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// TrimContext asks the run of the live hold id to lower its reservation to what the run uses and
// to follow it: inside the section it writes a request for id into trim.json, which the run reads
// at its tick. It refuses, writing nothing, a request that could do nothing: ErrNoHold when no
// live hold has that id, ErrNothingReserved for a hold that reserves nothing (a run outside the
// budget) and ErrCommandExited for one whose command has exited. A live hold whose body reads
// torn is found by its name and not refused. In a state directory whose format this client does
// not implement nothing is written and it returns an error matching ErrRefused. Its wait for the
// section ends with ctx, as CancelContext's does.
func (e *Engine) TrimContext(ctx context.Context, id wire.ID) error {
	sec, err := e.enterUntil(ctx)
	if err != nil {
		return err
	}
	defer sec.Leave()
	now := e.clock.Realtime() // reconciliation and the request read the same clock
	n, understood, err := sec.Prepare(now)
	if err != nil {
		return err
	}
	if !understood {
		return refuseFormat(n, "")
	}
	// A torn body reads as reserving 1 MiB: what it reserves is unknown, which is not nothing.
	live, dead, _, err := holdListing(e.st, 1)
	if err != nil {
		return err
	}
	listed := map[string]bool{}
	var target *wire.Hold
	for _, h := range live {
		hid, _ := wire.ParseName(h.name)
		listed[hid.String()] = true
		if hid == id {
			target = &h.body
		}
	}
	for _, name := range dead {
		hid, _ := wire.ParseName(name)
		listed[hid.String()] = true
	}
	switch {
	case target == nil:
		return ErrNoHold
	case target.NeedMiB == 0:
		return ErrNothingReserved
	case target.ChildExited:
		return ErrCommandExited
	}
	// Writing trim.json does not bump gen: no admission reads it.
	return sec.RequestTrim(id, store.TrimRequest{RequestedNS: max(now, 0), RequesterPID: os.Getpid()}, listed)
}

// TrimRequested reports the request in trim.json for this grant's own hold, read outside the
// section. A grant without a hold, a missing or unreadable file and a file without an entry for
// the hold all report none.
func (g *Grant) TrimRequested() (store.TrimRequest, bool) {
	id, ok := wire.ParseName(g.holdName)
	if !ok {
		return store.TrimRequest{}, false
	}
	reqs, err := g.e.st.ReadTrim()
	if err != nil {
		return store.TrimRequest{}, false
	}
	r, ok := reqs[id.String()]
	return r, ok
}
