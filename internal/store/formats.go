package store

import (
	"slices"

	"github.com/core-slot/coreslot/internal/testhooks"
)

// This file has no build constraint: `coreslot version`, which every platform builds, prints the
// same list the Unix layer acts on.

// Formats lists the protocol formats this client implements (PROTOCOL.md section 15), in the
// order `coreslot version` prints them: format 1, unless a build with the coreslot_testhooks tag
// is told otherwise.
func Formats() []int { return testhooks.Formats() }

// Understood reports whether this client implements format n. A FORMAT naming any other format is
// not understood: a client writes nothing into that directory and admits in token-only mode
// (PROTOCOL.md sections 14 and 15.2).
func Understood(n int) bool { return slices.Contains(Formats(), n) }
