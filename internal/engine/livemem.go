//go:build unix

package engine

import (
	"path/filepath"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/testhooks"
	"github.com/core-slot/coreslot/internal/wire"
)

// liveMem is the seam for the live capacity's readings; tests replace it. Under the test hook's
// headroom it measures no tree.
var liveMem = struct {
	headroom func() (mib int64, ok bool)
	use      func(roots map[int]int64, stop map[int]bool) map[int]int64 // nil map: nothing measured
}{
	headroom: func() (int64, bool) {
		if mib, set := testhooks.Headroom(); set {
			return mib, true
		}
		return memsample.Headroom()
	},
	use: func(roots map[int]int64, stop map[int]bool) map[int]int64 {
		if _, set := testhooks.Headroom(); set {
			return nil
		}
		procs, ok := memsample.Listing()
		if !ok {
			return nil
		}
		return memsample.HolderUse(procs, roots, stop)
	},
}

// capacityFor is mem's capacity, in MiB, for an attempt of need beside the live holds, which
// reserve reserved: the configured sizeMiB, unless live sizing is on and need does not fit in
// what the holds leave of it; then the reservations plus the room the holds leave of the
// headroom (state.Room), when that is more (raised). A failed headroom read keeps sizeMiB.
func capacityFor(sizeMiB int64, live bool, holds []memHold, reserved, need int64) (capacity int64, raised bool) {
	if !live || reserved+need <= sizeMiB {
		return sizeMiB, false
	}
	headroom, ok := liveMem.headroom()
	if !ok {
		return sizeMiB, false
	}
	if c := reserved + state.Room(headroom, measure(holds)); c > sizeMiB {
		return c, true
	}
	return sizeMiB, false
}

// memHold is one live hold as admission reads it.
type memHold struct {
	pid       int
	startNS   int64
	tree      bool
	needMiB   int64
	footprint int64 // MiB, as its run last wrote it
}

// reservedMiB is what holds reserve: the sum of their needs.
func reservedMiB(holds []memHold) int64 {
	var n int64
	for _, h := range holds {
		n += h.needMiB
	}
	return n
}

// liveHolds lists holds/ and returns the live holds, a lock probe each, as Ticket.Holders does;
// malformed and stray holds are left out. It fails when holds/ or a body cannot be read. It takes
// no lock of its own: admit runs under stepMu, which Ticket.Holders takes.
func liveHolds(st *store.Store) ([]memHold, error) {
	entries, _, err := st.List(store.HoldsDir)
	if err != nil {
		return nil, err
	}
	var live []memHold
	for _, en := range entries {
		body, err := st.ReadSmall(filepath.Join(store.HoldsDir, en.Name), wire.MaxBodyBytes)
		if gone(err) || stray(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		info := holderInfo(en, body)
		if info == nil {
			continue
		}
		dead, err := probe(st, store.HoldsDir, en.Name)
		if gone(err) || stray(err) || (err == nil && dead) {
			continue
		}
		if err != nil {
			return nil, err
		}
		live = append(live, memHold{pid: info.PID, startNS: info.StartNS, tree: info.Tree,
			needMiB: info.NeedMiB, footprint: info.FootprintMiB})
	}
	return live, nil
}

// measure turns holds into the room's input: a hold that reserves memory is measured, less every
// other live hold's subtree, when it alone claims its pid's tree; otherwise its footprint is the
// one its body carries.
func measure(holds []memHold) []state.MemHold {
	claims := make(map[int]int)
	stop := make(map[int]bool, len(holds))
	for _, h := range holds {
		stop[h.pid] = true
		if h.tree {
			claims[h.pid]++
		}
	}
	roots := make(map[int]int64)
	for _, h := range holds {
		if h.tree && claims[h.pid] == 1 && h.needMiB > 0 {
			roots[h.pid] = h.startNS
		}
	}
	var use map[int]int64
	if len(roots) > 0 {
		use = liveMem.use(roots, stop)
	}
	out := make([]state.MemHold, 0, len(holds))
	for _, h := range holds {
		if h.needMiB == 0 {
			continue
		}
		fp := h.footprint
		if kib, ok := use[h.pid]; ok && h.tree && claims[h.pid] == 1 {
			fp = kib / 1024
		}
		out = append(out, state.MemHold{NeedMiB: h.needMiB, FootprintMiB: fp})
	}
	return out
}
