package main

import "github.com/core-slot/coreslot/internal/needs"

// newNeedsLookup is parseRun's lookup of the needs file beside the configuration cfgPath. The
// file is loaded once, on first use. A file that cannot be read, or an entry that does not
// parse, is one notice and no entry: the run goes on as if the file had none.
func newNeedsLookup(cfgPath string, notice func(string, ...any)) func(string) (needs.Entry, bool) {
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
