//go:build unix

package main

import (
	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/store"
)

// openEngine opens the state directory as the library's Open does: CORESLOT_STATE_DIR (with
// its configuration at <statedir>/config.toml) or the default of PROTOCOL.md section 4.1.
func openEngine() (*engine.Engine, string, error) {
	dir, cfgPath, err := config.Locate()
	if err != nil {
		return nil, "", &stateError{Reason: err.Error()}
	}
	st, err := store.Open(dir)
	if err != nil {
		return nil, "", err
	}
	return engine.New(st, &config.Watcher{Path: cfgPath}, config.MachineCPUs, engine.Clock{}), cfgPath, nil
}

// engineStateReason is the engine's error contract (engine.StateReason), which stateReason
// applies to every error that is not the CLI's or the library's own state error.
func engineStateReason(err error) (string, bool) { return engine.StateReason(err) }
