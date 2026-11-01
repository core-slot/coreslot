// Package config reads coreslot's configuration and the machine's memory (PROTOCOL.md section 5).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"

	"github.com/BurntSushi/toml"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/testhooks"
)

// DefaultReserveMiB is the memory admission keeps free beyond every job's need. interim: the
// calibration sets it.
const DefaultReserveMiB = 1024

// Config is a parsed configuration file.
type Config struct {
	ReserveMiB     int64 // [memory] reserve
	WindowsWarning bool  // [platform] windows_warning, default true
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

// Machine is what admission reads of the machine: its physical memory and the memory the kernel
// can hand out without paging anything anonymous out, both in MiB. HeadroomOK is false when the
// headroom cannot be read.
type Machine struct {
	MemMiB      int64
	HeadroomMiB int64
	HeadroomOK  bool
}

// ReadMachine reads the machine's memory. The test hook's headroom replaces the kernel's.
func ReadMachine() (Machine, error) {
	total, err := memsample.Total()
	if err != nil {
		return Machine{}, err
	}
	m := Machine{MemMiB: total}
	if mib, set := testhooks.Headroom(); set {
		m.HeadroomMiB, m.HeadroomOK = mib, true
	} else {
		m.HeadroomMiB, m.HeadroomOK = memsample.Headroom()
	}
	return m, nil
}

// parseReserve parses reserve: a size spec.ParseBytes reads ("768M", "1.5G"), or an integer of
// GiB. 0 is allowed.
func parseReserve(v any) (int64, error) {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case int64:
		s = strconv.FormatInt(x, 10)
	default:
		return 0, fmt.Errorf(`reserve %v (%T): want an integer of GiB or a size such as "768M"`, v, v)
	}
	mib, err := spec.ParseBytes(s)
	if err != nil {
		return 0, fmt.Errorf("reserve: %v", err)
	}
	return mib, nil
}

func defaults() Config {
	return Config{ReserveMiB: DefaultReserveMiB, WindowsWarning: true}
}

// notRegularError is the reason of a configuration path that resolves to something but a regular
// file.
type notRegularError struct{ msg string }

func (e notRegularError) Error() string { return e.msg }

// Load reads the configuration at path, through symbolic links. A missing file, a dangling
// symlink included, is the defaults, with no error; an unreadable file, one that
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
		if r, ok := tbl["reserve"]; ok {
			mib, err := parseReserve(r)
			if err != nil {
				return Config{}, "[memory] " + err.Error()
			}
			c.ReserveMiB = mib
		}
	}
	return c, ""
}
