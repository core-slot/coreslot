// Package config reads coreslot's configuration and resolves resource sizes against the
// machine-wide CPU count (PROTOCOL.md section 5).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// Config is a parsed configuration file.
type Config struct {
	Resources      map[string]SizeExpr // declared resources; cpu is "100%" unless declared
	Priorities     map[string]int      // label prefix -> priority
	WindowsWarning bool                // [platform] windows_warning, default true
}

// Error reports a configuration that cannot be read or is invalid: a state error.
type Error struct {
	Path string
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("configuration %s: %s", e.Path, e.Msg) }

type sizeKind int

const (
	sizeAbsolute sizeKind = iota // an integer, used as given
	sizePercent                  // "N%": max(1, floor(cpus*N/100))
	sizeMinus                    // "-K": max(1, cpus-K)
)

// SizeExpr is a configured size; Resolve turns it into a size for a CPU count.
type SizeExpr struct {
	kind sizeKind
	n    int
}

var defaultCPU = SizeExpr{kind: sizePercent, n: 100}

// Resolve returns the size for cpus machine-wide online CPUs, within [1, spec.MaxSize].
func (s SizeExpr) Resolve(cpus int) int {
	v := s.n
	switch s.kind {
	case sizePercent:
		v = cpus * s.n / 100
	case sizeMinus:
		v = cpus - s.n
	}
	return min(max(v, 1), spec.MaxSize)
}

// ParseSize accepts an integer in [1, spec.MaxSize], a string "N%" with N in 1..100, or a string
// "-K" with K a non-negative integer. Anything else is an error.
func ParseSize(v any) (SizeExpr, error) {
	switch x := v.(type) {
	case int64:
		if x < 1 || x > spec.MaxSize {
			return SizeExpr{}, fmt.Errorf("size %d is outside [1, %d]", x, spec.MaxSize)
		}
		return SizeExpr{kind: sizeAbsolute, n: int(x)}, nil
	case int:
		return ParseSize(int64(x))
	case string:
		if pct, ok := strings.CutSuffix(x, "%"); ok {
			n, ok := digits(pct)
			if !ok || n < 1 || n > 100 {
				return SizeExpr{}, fmt.Errorf(`size %q: want "N%%" with N in 1..100`, x)
			}
			return SizeExpr{kind: sizePercent, n: n}, nil
		}
		if k, ok := strings.CutPrefix(x, "-"); ok {
			n, ok := digits(k)
			if !ok {
				return SizeExpr{}, fmt.Errorf(`size %q: want "-K" with K a non-negative integer`, x)
			}
			return SizeExpr{kind: sizeMinus, n: n}, nil
		}
		return SizeExpr{}, fmt.Errorf(`size %q: want an integer, "N%%" or "-K"`, x)
	default:
		return SizeExpr{}, fmt.Errorf(`size %v (%T): want an integer, "N%%" or "-K"`, v, v)
	}
}

// digits parses a non-empty string of at most 9 ASCII digits.
func digits(s string) (int, bool) {
	if s == "" || len(s) > 9 || strings.TrimLeft(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func defaults() Config {
	return Config{
		Resources:      map[string]SizeExpr{"cpu": defaultCPU},
		Priorities:     map[string]int{},
		WindowsWarning: true,
	}
}

// Load reads the configuration at path. A missing file is the defaults (cpu = "100%" only), with
// no error; an unreadable, malformed or invalid file is a *Error. Unknown tables and keys are
// ignored.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return defaults(), nil
	}
	if err != nil {
		return Config{}, &Error{Path: path, Msg: err.Error()}
	}
	c, msg := parse(string(data))
	if msg != "" {
		return Config{}, &Error{Path: path, Msg: msg}
	}
	return c, nil
}

func parse(data string) (Config, string) {
	var raw map[string]any
	if _, err := toml.Decode(data, &raw); err != nil {
		return Config{}, err.Error()
	}
	c := defaults()
	if v, ok := raw["resources"]; ok {
		tbl, ok := v.(map[string]any)
		if !ok {
			return Config{}, "[resources] is not a table"
		}
		for _, name := range slices.Sorted(maps.Keys(tbl)) {
			if err := spec.ValidateName(name); err != nil {
				return Config{}, "[resources]: " + err.Error()
			}
			size, err := ParseSize(tbl[name])
			if err != nil {
				return Config{}, fmt.Sprintf("[resources] %s: %v", name, err)
			}
			c.Resources[name] = size
		}
	}
	if v, ok := raw["priorities"]; ok {
		tbl, ok := v.(map[string]any)
		if !ok {
			return Config{}, "[priorities] is not a table"
		}
		for _, prefix := range slices.Sorted(maps.Keys(tbl)) {
			p, ok := tbl[prefix].(int64)
			if !ok || p < spec.MinPriority || p > spec.MaxPriority {
				return Config{}, fmt.Sprintf("[priorities] %q: want an integer in [%d, %d]",
					prefix, spec.MinPriority, spec.MaxPriority)
			}
			c.Priorities[prefix] = int(p)
		}
	}
	if v, ok := raw["platform"]; ok {
		tbl, ok := v.(map[string]any)
		if !ok {
			return Config{}, "[platform] is not a table"
		}
		if w, ok := tbl["windows_warning"]; ok {
			b, ok := w.(bool)
			if !ok {
				return Config{}, "[platform] windows_warning: want a boolean"
			}
			c.WindowsWarning = b
		}
	}
	return c, ""
}

// Sizes resolves every declared resource for cpus; the result always contains cpu.
func (c Config) Sizes(cpus int) map[string]int {
	sizes := make(map[string]int, len(c.Resources)+1)
	for name, s := range c.Resources {
		sizes[name] = s.Resolve(cpus)
	}
	if _, ok := sizes["cpu"]; !ok {
		sizes["cpu"] = defaultCPU.Resolve(cpus)
	}
	return sizes
}

// PriorityFor returns the priority of the longest configured prefix of label, and false when no
// prefix matches.
func (c Config) PriorityFor(label string) (int, bool) {
	best, found := "", false
	for prefix := range c.Priorities {
		if strings.HasPrefix(label, prefix) && (!found || len(prefix) > len(best)) {
			best, found = prefix, true
		}
	}
	if !found {
		return 0, false
	}
	return c.Priorities[best], true
}

// parseOnline counts the CPUs of a Linux CPU list such as "0-3,8-11\n" (8 CPUs). It lives here,
// not in the Linux file, so that it is tested on every platform.
func parseOnline(s string) (int, error) {
	list := strings.TrimSuffix(s, "\n")
	if list == "" {
		return 0, fmt.Errorf("empty CPU list")
	}
	total := 0
	for _, item := range strings.Split(list, ",") {
		lo, hi, isRange := strings.Cut(item, "-")
		a, ok := digits(lo)
		b := a
		if ok && isRange {
			b, ok = digits(hi)
		}
		if !ok || b < a {
			return 0, fmt.Errorf("malformed CPU list %q", s)
		}
		total += b - a + 1
	}
	return total, nil
}
