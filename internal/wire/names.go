// Package wire is the on-disk wire format of PROTOCOL.md format 1: the fixed-width names of
// tickets, holds and cancel requests, the strict JSON bodies they carry, and the escaping of
// user text. It does no I/O.
package wire

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
)

// Widths of the fields of a name (PROTOCOL.md section 7.1).
const (
	orderKeyDigits = 20
	pidDigits      = 10
	seqDigits      = 6
	idLen          = orderKeyDigits + 1 + pidDigits + 1 + seqDigits
	maxDisplay     = 32

	// SeqModulus is where a per-process seq counter wraps: 999999 is followed by 0.
	SeqModulus = 1_000_000
)

// ID is the identity of a ticket or hold: the first three fields of its name. For a ticket the
// first field is the order key, for a hold the start time in nanoseconds.
//
// PID must be below 10^10 and Seq below SeqModulus; String does not check, ParseName never
// returns an ID outside those bounds.
type ID struct {
	OrderKey uint64
	PID      int
	Seq      int
}

// String is the ID in name form, `%020d-%010d-%06d`: the byte order of the strings is the
// numeric order of (OrderKey, PID, Seq). It is also the name of a cancel request.
func (id ID) String() string {
	return fmt.Sprintf("%020d-%010d-%06d", id.OrderKey, id.PID, id.Seq)
}

// TicketName is the file name of a ticket: its ID, then a display derived from the label.
func TicketName(id ID, label string) string {
	return withDisplay(id, label)
}

// HoldName is the file name of a hold: its start time in nanoseconds in place of the order key.
func HoldName(startNS uint64, pid, seq int, label string) string {
	return withDisplay(ID{OrderKey: startNS, PID: pid, Seq: seq}, label)
}

func withDisplay(id ID, label string) string {
	if d := Display(label); d != "" {
		return id.String() + "-" + d
	}
	return id.String()
}

// NextSeq is the seq that follows s in a process's counter: it wraps from 999999 to 0.
func NextSeq(s int) int {
	return (s + 1) % SeqModulus
}

// ParseName parses a ticket or hold name: three fixed-width decimal fields, optionally followed
// by "-" and a display of 1 to 32 characters from [A-Za-z0-9._]. The display is checked against
// the grammar and otherwise ignored. Anything else, including a temporary file's name, is not a
// name.
func ParseName(name string) (ID, bool) {
	if len(name) < idLen {
		return ID{}, false
	}
	id, ok := ParseID(name[:idLen])
	if !ok {
		return ID{}, false
	}
	if len(name) == idLen {
		return id, true
	}
	if name[idLen] != '-' {
		return ID{}, false
	}
	d := name[idLen+1:]
	if len(d) < 1 || len(d) > maxDisplay {
		return ID{}, false
	}
	for i := 0; i < len(d); i++ {
		if !displayByte(d[i]) {
			return ID{}, false
		}
	}
	return id, true
}

// NameDisplay is the display part of a ticket or hold name (the label's informational
// suffix), or "" when the name has none or is not a name.
func NameDisplay(name string) string {
	if _, ok := ParseName(name); !ok || len(name) == idLen {
		return ""
	}
	return name[idLen+1:]
}

// ParseID parses exactly the three fields, the grammar of a cancel request's name.
func ParseID(name string) (ID, bool) {
	if len(name) != idLen || name[orderKeyDigits] != '-' || name[orderKeyDigits+1+pidDigits] != '-' {
		return ID{}, false
	}
	k, ok := digits(name[:orderKeyDigits])
	if !ok {
		return ID{}, false
	}
	pidStart := orderKeyDigits + 1
	seqStart := pidStart + pidDigits + 1
	pid, ok1 := digits(name[pidStart : pidStart+pidDigits])
	seq, ok2 := digits(name[seqStart:])
	if !ok1 || !ok2 || uint64(int(pid)) != pid {
		return ID{}, false
	}
	return ID{OrderKey: k, PID: int(pid), Seq: int(seq)}, true
}

// digits parses a field of ASCII decimal digits only, refusing signs, spaces and overflow.
func digits(s string) (uint64, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(s, 10, 64)
	return v, err == nil
}

// OrderKey is `arrival_ns - priority * 60_000_000_000` (PROTOCOL.md section 9): ascending order
// runs first, and a minute of waiting is worth one priority point. The result saturates: below
// zero it is 0 (the key is unsigned) and above the largest uint64 it is that maximum. The caller
// validates priority; no input makes the arithmetic wrap.
func OrderKey(arrivalNS int64, priority int) uint64 {
	k := new(big.Int).Mul(big.NewInt(int64(priority)), big.NewInt(60_000_000_000))
	k.Sub(big.NewInt(arrivalNS), k)
	switch {
	case k.Sign() < 0:
		return 0
	case !k.IsUint64():
		return math.MaxUint64
	}
	return k.Uint64()
}
