// Package spec holds the validated request type shared by the engine and the CLI: a memory need
// in MiB and a label, with the bounds of PROTOCOL.md.
package spec

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Bounds of format 2 (PROTOCOL.md section 7).
const (
	MaxLabelBytes = 128
	// MaxNeedMiB bounds every MiB amount of a body (1 PiB), so that summing a full directory of
	// them never overflows.
	MaxNeedMiB = 1 << 30
)

// ColdStartMiB is the need of a label with no pin and no counted run, on a machine of totalMiB.
func ColdStartMiB(totalMiB int64) int64 { return max(1, min(4096, totalMiB/4)) }

// Request is what a client asks for: NeedMiB of memory.
type Request struct {
	Label     string
	NeedMiB   int64
	ArrivalNS int64 // a stopped command's first arrival, which keeps its place; 0 arrives now
	Restart   bool  // a stopped command coming back: it waits for calm and a margin over its need
	TypicalS  int64 // the label's typical run time in whole seconds; 0 is unknown, which never passes
}

// UsageError reports input that can never be valid: the CLI's exit 64.
type UsageError struct {
	Msg string
}

func (e *UsageError) Error() string { return e.Msg }

func usagef(format string, args ...any) error {
	return &UsageError{Msg: fmt.Sprintf(format, args...)}
}

// ParseDigits parses one to nine ASCII decimal digits and nothing else: no sign, no space,
// leading zeros allowed. It is the number of the FORMAT line (PROTOCOL.md section 7.6); nine digits
// always fit an int.
func ParseDigits(s string) (int, bool) {
	if s == "" || len(s) > 9 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// Validate checks every bound of a request; a need of 0 is a *UsageError.
func (r Request) Validate() error {
	if r.NeedMiB < 1 || r.NeedMiB > MaxNeedMiB {
		return usagef("a need of %d MiB: want 1 to %d", r.NeedMiB, MaxNeedMiB)
	}
	return ValidateLabel(r.Label)
}

// ValidateLabel checks the label rule every request and hold body follows: valid UTF-8 of at
// most MaxLabelBytes bytes (PROTOCOL.md section 7.3). A label that breaks it is a *UsageError.
func ValidateLabel(label string) error {
	if len(label) > MaxLabelBytes {
		return usagef("label is %d bytes, at most %d", len(label), MaxLabelBytes)
	}
	if !utf8.ValidString(label) {
		return usagef("label is not valid UTF-8")
	}
	return nil
}
