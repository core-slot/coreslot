// Package config reads coreslot's configuration and resolves the memory budget against the
// machine's physical memory (PROTOCOL.md section 5).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
)

// Config is a parsed configuration file.
type Config struct {
	Mem            SizeExpr // [resources] mem, 60% of physical memory unless set
	WindowsWarning bool     // [platform] windows_warning, default true
	LiveMem        bool     // [memory] live, default false
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
	sizeMemPercent sizeKind = iota // N% of physical memory
	sizeMemMiB                     // n MiB
)

// SizeExpr is a configured size; Resolve turns it into a size for a machine.
type SizeExpr struct {
	kind sizeKind
	n    int
}

var defaultMem = SizeExpr{kind: sizeMemPercent, n: 60}

// Machine is what sizes resolve against: the physical memory in MiB.
type Machine struct {
	MemMiB int64
}

// ReadMachine reads the machine's physical memory.
func ReadMachine() (Machine, error) {
	mib, err := memsample.Total()
	if err != nil {
		return Machine{}, err
	}
	return Machine{MemMiB: mib}, nil
}

// Resolve returns the size in MiB on machine m, rounded down, at least 1.
func (s SizeExpr) Resolve(m Machine) int64 {
	v := int64(s.n)
	if s.kind == sizeMemPercent {
		v = m.MemMiB * v / 100
	}
	return max(v, 1)
}

// ParseMemSize parses the size of the mem resource: an integer is GiB; a string is "N%" of
// physical memory with N in 1..100, or a size spec.ParseBytes reads ("9.5G", "768M"), more than
// 0. Anything else is an error.
func ParseMemSize(v any) (SizeExpr, error) {
	switch x := v.(type) {
	case int64:
		if x < 0 {
			return SizeExpr{}, fmt.Errorf("mem %d is negative", x)
		}
		return ParseMemSize(strconv.FormatInt(x, 10))
	case string:
		if pct, ok := strings.CutSuffix(x, "%"); ok {
			n, ok := spec.ParseDigits(pct)
			if !ok || n < 1 || n > 100 {
				return SizeExpr{}, fmt.Errorf(`mem %q: want "N%%" with N in 1..100`, x)
			}
			return SizeExpr{kind: sizeMemPercent, n: n}, nil
		}
		mib, err := spec.ParseBytes(x)
		if err != nil {
			return SizeExpr{}, fmt.Errorf(`mem: %v, or "N%%" of memory`, err)
		}
		if mib == 0 {
			return SizeExpr{}, fmt.Errorf("mem %q: want more than 0", x)
		}
		return SizeExpr{kind: sizeMemMiB, n: int(mib)}, nil
	default:
		return SizeExpr{}, fmt.Errorf(`mem %v (%T): want an integer of GiB, "N%%" or a size such as "768M"`, v, v)
	}
}

func defaults() Config {
	return Config{Mem: defaultMem, WindowsWarning: true}
}

// notRegularError is the reason of a configuration path that resolves to something but a regular
// file.
type notRegularError struct{ msg string }

func (e notRegularError) Error() string { return e.msg }

// Load reads the configuration at path, through symbolic links. A missing file, a dangling
// symlink included, is the defaults (mem = "60%"), with no error; an unreadable file, one that
// is not a regular file (a directory, a FIFO, a device; Load never blocks on it), or a malformed
// or invalid one is a *Error. Unknown tables and keys are ignored.
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
		if v, ok := tbl["mem"]; ok {
			size, err := ParseMemSize(v)
			if err != nil {
				return Config{}, "[resources] " + err.Error()
			}
			c.Mem = size
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
	if v, ok := raw["memory"]; ok {
		tbl, ok := v.(map[string]any)
		if !ok {
			return Config{}, "[memory] is not a table"
		}
		if w, ok := tbl["live"]; ok {
			b, ok := w.(bool)
			if !ok {
				return Config{}, "[memory] live: want a boolean"
			}
			c.LiveMem = b
		}
	}
	return c, ""
}

// MemMiB resolves the budget on machine m, in MiB.
func (c Config) MemMiB(m Machine) int64 { return c.Mem.Resolve(m) }
