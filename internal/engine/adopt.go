//go:build unix

package engine

import (
	"context"

	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/state"
)

// AdoptContext admits already-running work without queueing (PROTOCOL.md section 12.4): inside
// the section it recomputes sizes, counts locked tokens at every index, and takes what is free
// under I3 in the two passes of section 12.1 step 6 (first up to min(min, size) of each
// requested declared resource, as many as are free, then more up to max while the grant holds
// fewer than 512 tokens), and publishes a hold with "adopted": true listing exactly what it
// took. It has no minimum: shortfall maps every requested declared resource to
// max(0, min - granted). In a state directory whose format this client does not implement it
// writes nothing and returns an error matching ErrRefused. Its wait for the section ends with
// ctx, as EnqueueContext's does: nothing is taken or published then.
func (e *Engine) AdoptContext(ctx context.Context, req spec.Request) (g *Grant, shortfall map[string]int, err error) {
	req = spec.Normalize(req)
	if err := req.Validate(); err != nil {
		return nil, nil, err
	}
	sec, err := e.enterUntil(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer sec.Leave()
	n, understood, err := sec.Prepare(e.clock.Realtime())
	if err != nil {
		return nil, nil, err
	}
	if !understood {
		return nil, nil, refuseFormat(n, "")
	}
	sizes, err := e.sizes()
	if err != nil {
		return nil, nil, err
	}
	eff, clamped, undeclared := state.Effective(req.Need, sizes)
	locked, err := countLocked(sec, eff)
	if err != nil {
		return nil, nil, err
	}
	take := state.AdoptPlan(eff, sizes, locked)
	mins := make(map[string]int, len(eff))
	for name, r := range eff {
		mins[name] = min(r.Min, take[name])
	}
	first, got, err := sec.TakeTokens(mins, sizes)
	if err != nil {
		return nil, nil, err
	}
	extra := make(map[string]int, len(eff))
	for name := range eff {
		extra[name] = max(0, take[name]-got[name])
	}
	second, _, err := sec.TakeTokens(extra, sizes)
	if err != nil {
		first.Close()
		return nil, nil, err
	}
	ts := mergeTokens(first, second)
	point("tokens.taken")
	if err := ts.CheckIdentity(); err != nil {
		ts.Close()
		return nil, nil, err
	}
	g = &Grant{e: e, tokens: ts, need: req.Need, clamped: clamped, unbudgeted: undeclared}
	if err := g.publishHold(sec, req.Label, true); err != nil {
		ts.Close()
		return nil, nil, err
	}
	_ = sec.BumpGen() // publishing a hold bumps gen once (PROTOCOL.md section 7.8)
	shortfall = make(map[string]int, len(eff))
	for name := range eff {
		shortfall[name] = max(0, req.Need[name].Min-len(ts.Indices[name]))
	}
	return g, shortfall, nil
}
