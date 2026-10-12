//go:build unix

package main

import (
	"errors"

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
		return nil, "", asState(err)
	}
	return engine.New(st, &config.Watcher{Path: cfgPath}, config.MachineCPUs, engine.Clock{}), cfgPath, nil
}

// asState applies the error contract the library applies (engine.StateReason): the classes the
// run acts on (usage, cancellation, context ends, the engine's refusals) stay as they are, and
// whatever else stops the budget from working becomes a state error, on which the run fails open
// (PROTOCOL.md section 14).
func asState(err error) error {
	var own *stateError
	if errors.As(err, &own) {
		return err
	}
	if reason, ok := engine.StateReason(err); ok {
		return &stateError{Reason: reason}
	}
	return err
}

func isStoreStateError(err error) bool {
	var se *store.StateError
	return errors.As(err, &se)
}
