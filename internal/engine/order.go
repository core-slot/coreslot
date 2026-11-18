//go:build unix

package engine

import (
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// orders reads order/ (PROTOCOL.md section 7.12): each well-formed override's effective order key
// by its ticket's id, and how many entries were ignored (malformed bodies and entries that do not
// match the grammar or the filesystem policy). A gone override is left out. An override names any
// ticket id: one whose ticket is gone orders nothing and waits for reconciliation.
func (e *Engine) orders() (map[wire.ID]uint64, int, error) {
	entries, ignored, err := e.st.List(store.OrderDir)
	if err != nil {
		return nil, 0, err
	}
	m := make(map[wire.ID]uint64, len(entries))
	for _, en := range entries {
		body, err := e.readBody(store.OrderDir, en.Name)
		switch {
		case gone(err):
			continue
		case stray(err):
			ignored++
			continue
		case err != nil:
			return nil, 0, err
		}
		o, err := wire.DecodeOrder(body)
		if err != nil {
			ignored++
			continue
		}
		m[en.ID] = uint64(o.OrderNS)
	}
	return m, ignored, nil
}

// orderKey is the effective order key of the ticket id: its override's when orders has one, its
// name's otherwise, with the name's pid and seq to break ties (PROTOCOL.md section 9).
func orderKey(id wire.ID, orders map[wire.ID]uint64) state.Key {
	k := state.Key(id)
	if n, ok := orders[id]; ok {
		k.OrderKey = n
	}
	return k
}
