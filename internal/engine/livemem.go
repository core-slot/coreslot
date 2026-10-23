//go:build unix

package engine

import (
	"maps"
	"path/filepath"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/testhooks"
	"github.com/core-slot/coreslot/internal/wire"
)

// liveMem is the seam for the live size's readings; tests replace it. Under the test hook's
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

// attemptSizes returns the sizes for this attempt: sizes unchanged unless the request's mem is
// fixed, live (the LiveMem of the configuration sizes came from) is on, mem is declared and
// sizes["mem"]-locked["mem"] is below its min; then mem's size is state.LiveMemSize over the live
// holds, read inside the section the caller is in. A failed headroom read or listing of holds/
// leaves sizes unchanged; a failed process listing or use read counts the holds it touches
// whole. It never returns an error.
func (e *Engine) attemptSizes(need map[string]spec.Range, sizes, locked map[string]int, live bool) (out map[string]int, raised bool) {
	r, requested := need["mem"]
	static, declared := sizes["mem"]
	if !live || !requested || !declared || r.Min != r.Max || static-locked["mem"] >= r.Min {
		return sizes, false
	}
	headroom, ok := liveMem.headroom()
	if !ok {
		return sizes, false
	}
	holds, ok := liveMemHolds(e.st)
	if !ok {
		return sizes, false
	}
	size, raised := state.LiveMemSize(static, locked["mem"], headroom, measure(holds))
	if !raised {
		return sizes, false
	}
	out = maps.Clone(sizes)
	out["mem"] = size
	return out, true
}

// memHold is one live hold as the live size reads it.
type memHold struct {
	pid     int
	startNS int64
	tree    bool
	tokens  int // mem tokens it lists
}

// liveMemHolds lists holds/ and returns the live holds, a lock probe each, as Ticket.Holders
// does; malformed and stray holds are left out, so their tokens count as unlisted. ok is false
// when holds/ or a body could not be read. It takes no lock of its own: admit runs under stepMu,
// which Ticket.Holders takes.
func liveMemHolds(st *store.Store) ([]memHold, bool) {
	entries, _, err := st.List(store.HoldsDir)
	if err != nil {
		return nil, false
	}
	var live []memHold
	for _, en := range entries {
		body, err := st.ReadSmall(filepath.Join(store.HoldsDir, en.Name), wire.MaxBodyBytes)
		if gone(err) || stray(err) {
			continue
		}
		if err != nil {
			return nil, false
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
			return nil, false
		}
		live = append(live, memHold{pid: info.PID, startNS: info.StartNS, tree: info.Tree, tokens: info.Grant["mem"]})
	}
	return live, true
}

// measure turns holds into the live size's input: a hold listing mem tokens is measured, less
// every other live hold's subtree, when it alone claims its pid's tree; else it counts whole.
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
		if h.tree && claims[h.pid] == 1 && h.tokens > 0 {
			roots[h.pid] = h.startNS
		}
	}
	var use map[int]int64
	if len(roots) > 0 {
		use = liveMem.use(roots, stop)
	}
	out := make([]state.MemHold, 0, len(holds))
	for _, h := range holds {
		if h.tokens == 0 {
			continue
		}
		used := int64(-1)
		if kib, ok := use[h.pid]; ok && h.tree && claims[h.pid] == 1 {
			used = kib
		}
		out = append(out, state.MemHold{Tokens: h.tokens, UsedKiB: used})
	}
	return out
}
