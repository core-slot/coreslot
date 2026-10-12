// Package spec holds the validated request types shared by the library, the engine and the CLI:
// resource names, need ranges, priorities and labels, with the bounds of PROTOCOL.md.
package spec

import (
	"fmt"
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

// ParseNeed parses one need argument, `RES=MIN..MAX` or `RES=N` (meaning N..N).
func ParseNeed(arg string) (string, Range, error) {
	name, spec, ok := strings.Cut(arg, "=")
	if !ok {
		return "", Range{}, usagef("invalid need %q: want RES=MIN..MAX or RES=N", arg)
	}
	if err := ValidateName(name); err != nil {
		return "", Range{}, usagef("invalid need %q: %v", arg, err)
	}
	lo, hi, isRange := strings.Cut(spec, "..")
	if !isRange {
		hi = lo
	}
	low, err := parseCount(lo)
	if err != nil {
		return "", Range{}, usagef("invalid need %q: %v", arg, err)
	}
	high, err := parseCount(hi)
	if err != nil {
		return "", Range{}, usagef("invalid need %q: %v", arg, err)
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

// Validate checks every bound of a request. Call it on a normalized request.
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
	need := make(map[string]Range, len(r.Need))
	for name, rg := range r.Need {
		need[name] = rg
	}
	if len(need) == 0 {
		need["cpu"] = Range{Min: 1, Max: 1}
	}
	r.Need = need
	return r
}
