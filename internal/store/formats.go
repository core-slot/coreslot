package store

// This file has no build constraint: `coreslot version`, which every platform builds, prints the
// format the Unix layer acts on.

// Format is the protocol format this client implements and writes (PROTOCOL.md section 15).
const Format = 5

// Upgraded reports whether a reset upgrades format n as a repair, keeping the run log and the
// telemetry (PROTOCOL.md section 4.4): format 4, which format 5 extends only by watch-mode, and
// format 3, which format 4 extended only by vmwait/ and the optional Docker keys of tickets and
// holds.
func Upgraded(n int) bool { return n == 4 || n == 3 }

// Understood reports whether this client implements format n. A FORMAT naming any other format is
// not understood: a client writes nothing into that directory, and runs fail open until
// `coreslot doctor --reset` recreates it (PROTOCOL.md sections 14 and 15.2).
func Understood(n int) bool { return n == Format }
