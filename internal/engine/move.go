//go:build unix

package engine

import (
	"context"
	"math"

	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// Place says where a move puts its ticket (PROTOCOL.md section 13.3).
type Place int

const (
	PlaceFirst  Place = iota + 1 // ahead of every ticket in the queue
	PlaceLast                    // behind every ticket in the queue
	PlaceBefore                  // just ahead of a named ticket
	PlaceAfter                   // just behind a named ticket
)

// MoveContext moves the waiting ticket id to place inside the section (PROTOCOL.md section 13.3):
// it publishes the override order/<id> carrying the ticket's new effective order key, computed
// from the effective keys of the tickets in queue/ (ref names the ticket of PlaceBefore and
// PlaceAfter), replacing an earlier override of id, and bumps gen. The ticket itself is never
// renamed or rewritten. It returns the key written. When id, or ref, names no ticket in the queue
// nothing is written and it returns ErrNoTicket. Format, section and ctx are handled as by
// CancelContext.
func (e *Engine) MoveContext(ctx context.Context, id wire.ID, place Place, ref wire.ID) (orderNS int64, err error) {
	sec, err := e.enterUntil(ctx)
	if err != nil {
		return 0, err
	}
	defer sec.Leave()
	n, understood, err := sec.Prepare(e.clock.Realtime())
	if err != nil {
		return 0, err
	}
	if !understood {
		return 0, refuseFormat(n, "")
	}
	entries, _, err := e.st.List(store.QueueDir)
	if err != nil {
		return 0, err
	}
	orders, _, err := e.orders()
	if err != nil {
		return 0, err
	}
	var target, refKey *state.Key
	first, last, others := uint64(math.MaxUint64), uint64(0), 0
	for _, en := range entries {
		k := orderKey(en.ID, orders)
		if en.ID == id {
			target = &k
			continue // its own place is no bound for --first or --last
		}
		if en.ID == ref {
			refKey = &k
		}
		first, last, others = min(first, k.OrderKey), max(last, k.OrderKey), others+1
	}
	if target == nil || (place == PlaceBefore || place == PlaceAfter) && refKey == nil {
		return 0, ErrNoTicket
	}
	to := target.OrderKey // alone in the queue, it is already first and last
	switch {
	case place == PlaceFirst && others > 0:
		to = sub1(first)
	case place == PlaceLast && others > 0:
		to = min(last, math.MaxInt64-1) + 1 // a body carries at most 2^63-1
	case place == PlaceBefore:
		to = sub1(refKey.OrderKey)
	case place == PlaceAfter:
		to = min(refKey.OrderKey, math.MaxInt64-1) + 1
	}
	orderNS = int64(min(to, math.MaxInt64))
	body, err := wire.Encode(wire.Order{OrderNS: orderNS})
	if err != nil {
		return 0, err
	}
	if err := sec.PublishOrder(id, body); err != nil {
		return 0, err
	}
	return orderNS, sec.BumpGen()
}

// sub1 is the key just ahead of k, clamped at 0.
func sub1(k uint64) uint64 {
	if k == 0 {
		return 0
	}
	return k - 1
}
