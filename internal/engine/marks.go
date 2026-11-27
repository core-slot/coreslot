//go:build unix

package engine

import (
	"context"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// MarkContext marks the live ticket or live hold id inside the section (PROTOCOL.md section 7.18):
// dir is store.QuietDir or store.NoStopDir. already reports a mark that was there. With no live
// ticket or hold of that id nothing is published and it returns ErrNoTicket; the other results are
// CancelContext's.
func (e *Engine) MarkContext(ctx context.Context, dir string, id wire.ID) (already bool, err error) {
	sec, _, err := e.enterPrepared(ctx)
	if err != nil {
		return false, err
	}
	defer sec.Leave()
	if live, _, err := e.liveID(id); err != nil || !live {
		if err == nil {
			err = ErrNoTicket
		}
		return false, err
	}
	return sec.Mark(dir, id)
}

// UnmarkContext removes the mark dir/<id> inside the section. When id names no live ticket or hold
// (a ticket admitted meanwhile), it also removes the mark of every live hold of the ticket's run:
// the same pid, and the ticket's order key as its arrival (after a restart a descendant can keep the
// first attempt's hold live beside the second's). removed counts the marks removed; none is not an
// error.
func (e *Engine) UnmarkContext(ctx context.Context, dir string, id wire.ID) (removed int, err error) {
	sec, _, err := e.enterPrepared(ctx)
	if err != nil {
		return 0, err
	}
	defer sec.Leave()
	ids := []wire.ID{id}
	if live, holds, err := e.liveID(id); err != nil {
		return 0, err
	} else if !live {
		for _, h := range holds {
			if h.body.PID == id.PID && h.body.ArrivalNS == int64(id.OrderKey) {
				hid, _ := wire.ParseName(h.name)
				ids = append(ids, hid)
			}
		}
	}
	for _, id := range ids {
		r, err := sec.Unmark(dir, id)
		if err != nil {
			return removed, err
		}
		if r {
			removed++
		}
	}
	return removed, nil
}

// enterPrepared enters the section within ctx for a request or a mark and reconciles at now, the
// clock the request then reads; a format this client does not implement is refused.
func (e *Engine) enterPrepared(ctx context.Context) (*store.Section, int64, error) {
	sec, err := e.enterUntil(ctx)
	if err != nil {
		return nil, 0, err
	}
	now := e.clock.Realtime()
	n, understood, err := sec.Prepare(now)
	if err == nil && !understood {
		err = refuseFormat(n, "")
	}
	if err != nil {
		sec.Leave()
		return nil, 0, err
	}
	return sec, now, nil
}

// liveID reports whether id names a live ticket or a live hold, and returns the live holds.
func (e *Engine) liveID(id wire.ID) (live bool, holds []liveHold, err error) {
	if holds, _, _, err = holdListing(e.st, 0); err != nil { // the need is not read
		return false, nil, err
	}
	for _, h := range holds {
		if hid, _ := wire.ParseName(h.name); hid == id {
			return true, holds, nil
		}
	}
	entries, _, err := e.st.List(store.QueueDir)
	if err != nil {
		return false, nil, err
	}
	for _, en := range entries {
		if en.ID == id {
			dead, err := probe(e.st, store.QueueDir, en.Name)
			if err == nil && !dead {
				return true, holds, nil
			} else if err != nil && !gone(err) && !stray(err) {
				return false, nil, err
			}
		}
	}
	return false, holds, nil
}

// NoStop reports whether the grant's hold is marked do-not-stop (PROTOCOL.md section 7.18), read
// without the section; a mark directory that cannot be read reads as no mark. The control it serves
// has no adversary: it is the user's choice on the user's machine. Its anchor is nostop/<hold id>,
// written by `coreslot no-stop` or moved there by this run at admission, which this run reads
// before every write of its hold and again before it acts on a stop round; its cost is what the
// machine does without coreslot, where nothing is stopped.
func (g *Grant) NoStop() bool {
	id, ok := wire.ParseName(g.holdName)
	if !ok {
		return false
	}
	marks, err := g.e.st.Marks(store.NoStopDir)
	return err == nil && marks[id.String()]
}
