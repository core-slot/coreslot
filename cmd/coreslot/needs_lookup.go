package main

import (
	"github.com/core-slot/coreslot/internal/needs"
	"github.com/core-slot/coreslot/internal/spec"
)

// needsLookup finds the needs file's entry for a label.
type needsLookup func(label string) (e needs.Entry, ok bool)

// newNeedsLookup is resolveNeed's lookup of the needs file beside the configuration cfgPath. The
// file is loaded once, on first use. A file that cannot be read, or an entry that does not
// parse, is one notice and no entry: the run goes on as if the file had none.
func newNeedsLookup(cfgPath string, notice func(string, ...any)) needsLookup {
	path := needs.PathFor(cfgPath)
	var (
		f      needs.File
		err    error
		loaded bool
	)
	return func(label string) (needs.Entry, bool) {
		if !loaded {
			loaded = true
			if f, err = needs.Load(path); err != nil {
				notice("needs file %s: %s; ignoring it", text(path), text(err.Error()))
			}
		}
		if err != nil {
			return needs.Entry{}, false
		}
		e, ok, lerr := f.Lookup(label)
		if lerr != nil {
			notice("needs file %s: %s; ignoring it", text(path), text(lerr.Error()))
			return needs.Entry{}, false
		}
		return e, ok
	}
}

// resolveNeed sets the run's need: --mem, else the needs file's pin for its label, else what its
// last runs needed (learned, read only when neither is set), else the cold start of a machine of
// totalMiB. It also takes the label's no_restart mark.
func (o *runOptions) resolveNeed(lookup needsLookup, learned func() (int64, bool), totalMiB int64) {
	var e needs.Entry
	if lookup != nil && o.label != "" {
		e, _ = lookup(o.label)
	}
	o.noRestart = e.NoRestart
	if mib := o.mem; mib > 0 {
		o.need, o.pinned = mib, true
		return
	}
	if e.MemMiB > 0 {
		o.need, o.pinned = e.MemMiB, true
		return
	}
	if mib, ok := learned(); ok {
		o.need = mib
		return
	}
	o.need = spec.ColdStartMiB(totalMiB)
}
