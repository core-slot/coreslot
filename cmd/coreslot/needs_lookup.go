package main

import "github.com/core-slot/coreslot/internal/needs"

// needsLookup finds the needs file's entry for a label; ignore, when the entry made the request
// invalid, prints the notice that sets it aside.
type needsLookup func(label string) (e needs.Entry, ok bool, ignore func(error))

// newNeedsLookup is parseRun's lookup of the needs file beside the configuration cfgPath. The
// file is loaded once, on first use. A file that cannot be read, or an entry that does not
// parse, is one notice and no entry: the run goes on as if the file had none.
func newNeedsLookup(cfgPath string, notice func(string, ...any)) needsLookup {
	path := needs.PathFor(cfgPath)
	var (
		f      needs.File
		err    error
		loaded bool
	)
	return func(label string) (needs.Entry, bool, func(error)) {
		if !loaded {
			loaded = true
			if f, err = needs.Load(path); err != nil {
				notice("needs file %s: %s; ignoring it", text(path), text(err.Error()))
			}
		}
		if err != nil {
			return needs.Entry{}, false, nil
		}
		e, ok, lerr := f.Lookup(label)
		if lerr != nil {
			notice("needs file %s: %s; ignoring it", text(path), text(lerr.Error()))
			return needs.Entry{}, false, nil
		}
		return e, ok, func(err error) {
			notice("needs file %s: the entry for %s makes the request invalid: %s; running without it",
				text(path), text(label), text(err.Error()))
		}
	}
}
