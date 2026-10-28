// Package spec holds the validated request types shared by the library, the engine and the CLI:
// resource names, need ranges, priorities and labels, with the bounds of PROTOCOL.md.
package spec

import (
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Bounds of format 1 (PROTOCOL.md sections 5, 6 and 7).
const (
	MaxSize        = 4096 // largest configured size of a resource
	MaxPerResource = 256  // largest request max, and most tokens of one resource in a grant
	MaxPerGrant    = 512  // most tokens in one grant
	MaxResources   = 8    // most resources in one request
	MinPriority    = -1000
	MaxPriority    = 1000
	MaxLabelBytes  = 128

	// DefaultMax is the max the CLI fills in where a request leaves it open: `RES=MIN..` means
	// MIN..max(MIN, DefaultMax), and a run without --need whose command takes a cpu placeholder
	// asks for cpu=1..DefaultMax. Admission caps a max at the resource's size anyway, so it
	// only has to be large enough not to leave a typical machine's cores idle.
	DefaultMax = 16
)

// Range is a request for one resource: start once Min is free, take up to Max.
type Range struct {
	Min, Max int
}

// Request is what a client asks for. Need maps a resource name to its range.
type Request struct {
	Label    string
	Priority int
	Need     map[string]Range
	Tree     bool // the process tree at the requester's pid carries the grant's work; only the CLI sets it
	// Preemptible is why the run is preemptible (the run log's reason), "" when it is not; only the
	// CLI sets it, and the hold carries it for status.
	Preemptible string
}

// UsageError reports input that can never be valid: the CLI's exit 64.
type UsageError struct {
	Msg string
}

func (e *UsageError) Error() string { return e.Msg }

func usagef(format string, args ...any) error {
	return &UsageError{Msg: fmt.Sprintf(format, args...)}
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidateName checks a resource name against `[a-z][a-z0-9-]{0,31}`.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return usagef("invalid resource name %q: want [a-z][a-z0-9-]{0,31}", name)
	}
	return nil
}

// EnvName is a resource name's environment form: upper case, "-" mapped to "_".
func EnvName(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// ParseNeed parses one need argument: `RES=MIN..MAX`, `RES=N` (meaning N..N), `RES=MIN..`
// (meaning MIN..max(MIN, DefaultMax)) or `RES=..MAX` (meaning 1..MAX). A range names at least
// one end, so `RES=..` is malformed. The mem resource takes one size instead, `mem=3G` or
// `mem=768M`, which is a fixed range of 128 MiB tokens rounded up.
func ParseNeed(arg string) (string, Range, error) {
	name, spec, ok := strings.Cut(arg, "=")
	if !ok {
		return "", Range{}, usagef("invalid need %q: want RES=MIN..MAX, RES=MIN.., RES=..MAX or RES=N", arg)
	}
	if err := ValidateName(name); err != nil {
		return "", Range{}, usagef("invalid need %q: %v", arg, err)
	}
	if name == "mem" {
		r, err := parseMemNeed(arg, spec)
		if err != nil {
			return "", Range{}, err
		}
		return name, r, nil
	}
	lo, hi, isRange := strings.Cut(spec, "..")
	switch {
	case !isRange:
		hi = lo
	case lo == "" && hi == "":
		return "", Range{}, usagef("invalid need %q: a range names its min, its max or both", arg)
	}
	low := 1 // `RES=..MAX`: any number of tokens from one up
	if !isRange || lo != "" {
		var err error
		if low, err = parseCount(lo); err != nil {
			return "", Range{}, usagef("invalid need %q: %v", arg, err)
		}
	}
	high := max(low, DefaultMax) // `RES=MIN..`: as many as admission allows, up to DefaultMax
	if hi != "" || !isRange {
		var err error
		if high, err = parseCount(hi); err != nil {
			return "", Range{}, usagef("invalid need %q: %v", arg, err)
		}
	}
	r := Range{Min: low, Max: high}
	if err := r.validate(name); err != nil {
		return "", Range{}, err
	}
	return name, r, nil
}

// ParseNeeds parses every need argument of one request and refuses a resource named twice.
func ParseNeeds(args []string) (map[string]Range, error) {
	need := make(map[string]Range, len(args))
	for _, arg := range args {
		name, r, err := ParseNeed(arg)
		if err != nil {
			return nil, err
		}
		if _, dup := need[name]; dup {
			return nil, usagef("resource %q is named more than once", name)
		}
		need[name] = r
	}
	return need, nil
}

// ParseDigits parses one to nine ASCII decimal digits and nothing else: no sign, no space,
// leading zeros allowed. It is the number of the FORMAT line (PROTOCOL.md section 7.6), of a
// descriptor in CORESLOT_HELD_FD and CORESLOT_TOKEN_FDS (section 17), and of the configuration's
// sizes and the CPU list; nine digits always fit an int.
func ParseDigits(s string) (int, bool) {
	if s == "" || len(s) > 9 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// parseCount accepts decimal digits only: no sign, no spaces.
func parseCount(s string) (int, error) {
	if s == "" || len(s) > 6 || strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not a count", s)
	}
	return strconv.Atoi(s)
}

func (r Range) validate(name string) error {
	if r.Min < 1 || r.Min > r.Max || r.Max > MaxPerResource {
		return usagef("invalid range %d..%d for %s: want 1 <= min <= max <= %d", r.Min, r.Max, name, MaxPerResource)
	}
	return nil
}

// Validate checks every bound of a request. Call it on a normalized request: an empty Need is a
// *UsageError here, where Normalize makes it cpu=1..1.
func (r Request) Validate() error {
	if len(r.Need) == 0 {
		return usagef("a request names at least one resource")
	}
	if len(r.Need) > MaxResources {
		return usagef("a request names at most %d resources, got %d", MaxResources, len(r.Need))
	}
	minSum := 0
	for name, rg := range r.Need {
		if err := ValidateName(name); err != nil {
			return err
		}
		if err := rg.validate(name); err != nil {
			return err
		}
		minSum += rg.Min
	}
	// A grant holds at most MaxPerGrant tokens, so minimums above it could never be admitted.
	if minSum > MaxPerGrant {
		return usagef("the minimums sum to %d, a grant holds at most %d tokens", minSum, MaxPerGrant)
	}
	if r.Priority < MinPriority || r.Priority > MaxPriority {
		return usagef("priority %d is outside [%d, %d]", r.Priority, MinPriority, MaxPriority)
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

// Normalize returns a copy of r whose Need defaults to cpu=1..1 when empty. It never shares the
// caller's map.
func Normalize(r Request) Request {
	if len(r.Need) == 0 {
		r.Need = map[string]Range{"cpu": {Min: 1, Max: 1}}
	} else {
		r.Need = maps.Clone(r.Need)
	}
	return r
}
