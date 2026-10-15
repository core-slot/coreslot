//go:build unix

package main

import (
	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/store"
)

// location is what config.Locate returned. A command locates once: each lookup of the home
// directory may start getent.
type location struct {
	dir, cfgPath string
	err          error
}

// locate finds the state directory as the library's Open does: CORESLOT_STATE_DIR (with its
// configuration at <statedir>/config.toml) or the default of PROTOCOL.md section 4.1.
func locate() location {
	var loc location
	loc.dir, loc.cfgPath, loc.err = config.Locate()
	return loc
}

// openEngine opens the state directory at loc. A run creates what is missing of it; readOnly
// creates nothing, not the directory, res/ or admit.lock (status, cancel and doctor leave a
// machine where nothing has run as they found it), and the engine's Snapshot then reports a
// missing directory as empty.
func openEngine(loc location, readOnly bool) (*engine.Engine, error) {
	if loc.err != nil {
		return nil, &stateError{Reason: loc.err.Error(), Err: loc.err}
	}
	open := store.Open
	if readOnly {
		open = store.OpenReadOnly
	}
	st, err := open(loc.dir)
	if err != nil {
		return nil, asState(err)
	}
	return engine.New(st, &config.Watcher{Path: loc.cfgPath}, config.MachineCPUs, engine.Clock{}), nil
}

// asState gives openEngine's failure the CLI's own form, "state error: <reason>" with one
// prefix, for the messages status, cancel and doctor print (a run prints the reason alone).
// Classification needs no wrapping: classify judges every error the same way (stateReason).
func asState(err error) error {
	if reason, ok := stateReason(err); ok {
		return &stateError{Reason: reason, Err: err}
	}
	return err
}

// engineStateReason is the engine's error contract (engine.StateReason), which stateReason
// applies to every error that is not the CLI's or the library's own state error.
func engineStateReason(err error) (string, bool) { return engine.StateReason(err) }
