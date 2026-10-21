// Package config reads coreslot's configuration and resolves resource sizes against the
// machine-wide CPU count and physical memory (PROTOCOL.md section 5).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
)

// Config is a parsed configuration file.
type Config struct {
	Resources      map[string]SizeExpr // declared resources; cpu is "140%" and mem "60%" unless declared
	Priorities     map[string]int      // label prefix -> priority
	WindowsWarning bool                // [platform] windows_warning, default true
}

// Error reports a configuration that cannot be read or is invalid: a state error. Err is the
// error that made it unreadable, nil for an invalid one; errors.Is and errors.As see through it.
type Error struct {
	Path string
	Msg  string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("configuration %s: %s", e.Path, e.Msg) }

// Unwrap returns the error that made the configuration unreadable, or nil.
func (e *Error) Unwrap() error { return e.Err }

type sizeKind int

const (
	sizeAbsolute   sizeKind = iota // an integer, used as given
	sizePercent                    // "N%": max(1, floor(cpus*N/100))
	sizeMinus                      // "-K": max(1, cpus-K)
	sizeMemPercent                 // mem "N%": N% of physical memory, in whole mem tokens
	sizeMemMiB                     // mem of n MiB, in whole mem tokens
)

// SizeExpr is a configured size; Resolve turns it into a size for a machine.
type SizeExpr struct {
	kind sizeKind
	n    int
}

var (
	defaultCPU = SizeExpr{kind: sizePercent, n: 140}
	defaultMem = SizeExpr{kind: sizeMemPercent, n: 60}
)

// Machine is what sizes resolve against: the machine-wide online CPU count and the physical
// memory in MiB.
type Machine struct {
	CPUs   int
	MemMiB int64
}

// ReadMachine reads the machine's CPU count, and its physical memory when mem: a configuration
// that does not declare mem resolves on a machine whose memory cannot be read.
func ReadMachine(mem bool) (Machine, error) {
	cpus, err := MachineCPUs()
	if err != nil || !mem {
		return Machine{CPUs: cpus}, err
	}
	mib, err := memsample.Total()
	if err != nil {
		return Machine{}, err
	}
	return Machine{CPUs: cpus, MemMiB: mib}, nil
}

// Resolve returns the size on machine m, within [1, spec.MaxSize]. A memory size is in mem
// tokens, rounded down.
func (s SizeExpr) Resolve(m Machine) int {
	v := s.n
	switch s.kind {
	case sizePercent:
		v = m.CPUs * s.n / 100
	case sizeMinus:
		v = m.CPUs - s.n
	case sizeMemPercent:
		v = spec.MemTokensFloor(m.MemMiB * int64(s.n) / 100)
	case sizeMemMiB:
		v = spec.MemTokensFloor(int64(s.n))
	}
	return min(max(v, 1), spec.MaxSize)
}

// ParseMemSize parses the size of the mem resource: an integer is GiB and 0 turns memory
// admission off; a string is "N%" of physical memory with N in 1..100, or a size spec.ParseBytes
// reads ("9.5G", "768M"), where a size of 0 is off too. Anything else is an error.
func ParseMemSize(v any) (size SizeExpr, off bool, err error) {
	switch x := v.(type) {
	case int64:
		if x < 0 {
			return SizeExpr{}, false, fmt.Errorf("mem %d is negative", x)
		}
		return ParseMemSize(strconv.FormatInt(x, 10))
	case string:
		if pct, ok := strings.CutSuffix(x, "%"); ok {
			n, ok := spec.ParseDigits(pct)
			if !ok || n < 1 || n > 100 {
				return SizeExpr{}, false, fmt.Errorf(`mem %q: want "N%%" with N in 1..100`, x)
			}
			return SizeExpr{kind: sizeMemPercent, n: n}, false, nil
		}
		mib, err := spec.ParseBytes(x)
		if err != nil {
			return SizeExpr{}, false, fmt.Errorf(`mem: %v, or "N%%" of memory`, err)
		}
		if mib == 0 {
			return SizeExpr{}, true, nil
		}
		return SizeExpr{kind: sizeMemMiB, n: int(mib)}, false, nil
	default:
		return SizeExpr{}, false, fmt.Errorf(`mem %v (%T): want an integer of GiB, "N%%" or a size such as "768M"`, v, v)
	}
}

// ParseSize accepts an integer in [1, spec.MaxSize], a string "N%" with N in 1..400, or a string
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
			n, ok := spec.ParseDigits(pct)
			if !ok || n < 1 || n > 400 {
				return SizeExpr{}, fmt.Errorf(`size %q: want "N%%" with N in 1..400`, x)
			}
			return SizeExpr{kind: sizePercent, n: n}, nil
		}
		if k, ok := strings.CutPrefix(x, "-"); ok {
			n, ok := spec.ParseDigits(k)
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

func defaults() Config {
	return Config{
		Resources:      map[string]SizeExpr{"cpu": defaultCPU, "mem": defaultMem},
		Priorities:     map[string]int{},
		WindowsWarning: true,
	}
}

// notRegularError is the reason of a configuration path that resolves to something but a regular
// file.
type notRegularError struct{ msg string }

func (e notRegularError) Error() string { return e.msg }

// Load reads the configuration at path, through symbolic links. A missing file, a dangling
// symlink included, is the defaults (cpu = "140%" and mem = "60%"), with no error; an unreadable file, one
// that is not a regular file (a directory, a FIFO, a device; Load never blocks on it), or a
// malformed or invalid one is a *Error. Unknown tables and keys are ignored.
func Load(path string) (Config, error) {
	data, err := readRegular(path)
	if errors.Is(err, fs.ErrNotExist) {
		return defaults(), nil
	}
	if err != nil {
		return Config{}, &Error{Path: path, Msg: err.Error(), Err: err}
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
			if name == "mem" {
				size, off, err := ParseMemSize(tbl[name])
				if err != nil {
					return Config{}, "[resources] " + err.Error()
				}
				if off {
					delete(c.Resources, name)
				} else {
					c.Resources[name] = size
				}
				continue
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

// Sizes resolves every declared resource on machine m; the result always contains cpu, and
// mem when it is declared.
func (c Config) Sizes(m Machine) map[string]int {
	sizes := make(map[string]int, len(c.Resources)+1)
	for name, s := range c.Resources {
		sizes[name] = s.Resolve(m)
	}
	if _, ok := sizes["cpu"]; !ok {
		sizes["cpu"] = defaultCPU.Resolve(m)
	}
	return sizes
}

// DeclaresMem reports whether the configuration declares mem: only then do sizes need the
// machine's memory.
func (c Config) DeclaresMem() bool {
	_, ok := c.Resources["mem"]
	return ok
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
		a, ok := spec.ParseDigits(lo)
		b := a
		if ok && isRange {
			b, ok = spec.ParseDigits(hi)
		}
		if !ok || b < a {
			return 0, fmt.Errorf("malformed CPU list %q", s)
		}
		total += b - a + 1
	}
	return total, nil
}
