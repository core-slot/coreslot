// Package needs reads and writes the needs file: one per machine, beside the configuration, with
// what a labelled run needs when its --need leaves a resource out.
package needs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/core-slot/coreslot/internal/spec"
)

// PathFor is the needs file that goes with the configuration file cfgPath.
func PathFor(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "needs.toml")
}

// BaseLabel is label without a final worker count: "@" followed by digits, or by "-".
func BaseLabel(label string) string {
	i := strings.LastIndexByte(label, '@')
	if i < 0 {
		return label
	}
	if suffix := label[i+1:]; suffix == "-" || (suffix != "" && strings.Trim(suffix, "0123456789") == "") {
		return label[:i]
	}
	return label
}

// Entry is what the needs file says about one label. CPU is nil and MemMiB 0 when the entry
// leaves that resource out.
type Entry struct {
	CPU       *spec.Range
	MemMiB    int64
	Pinned    bool // the entry is the user's, and is never rewritten from measurements
	PerWorker bool // the entry gives cpu per worker (cpu_per_worker)
}

// File is a loaded needs file.
type File struct {
	entries map[string]any
}

// Load reads the needs file at path, through symbolic links. A missing file is an empty File.
func Load(path string) (File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return File{}, err
	}
	entries, err := needsTable(raw)
	if err != nil {
		return File{}, err
	}
	return File{entries: entries}, nil
}

// FromEntries is the File whose [needs] table is entries, as Update hands them to its edit.
func FromEntries(entries map[string]map[string]any) File {
	f := File{entries: make(map[string]any, len(entries))}
	for label, e := range entries {
		f.entries[label] = e
	}
	return f
}

// needsTable is the [needs] table of a decoded file, nil when it has none.
func needsTable(raw map[string]any) (map[string]any, error) {
	v, ok := raw["needs"]
	if !ok {
		return nil, nil
	}
	tbl, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("[needs] is not a table")
	}
	return tbl, nil
}

// Lookup returns the entry for label's base (BaseLabel). An empty base finds nothing. cpu is a
// need's value, a string or an integer; mem is a size, a string or an integer of GiB. Unknown
// keys are ignored; a known key with a bad value is an error naming the label and the key.
func (f File) Lookup(label string) (Entry, bool, error) {
	base := BaseLabel(label)
	if base == "" {
		return Entry{}, false, nil
	}
	v, ok := f.entries[base]
	if !ok {
		return Entry{}, false, nil
	}
	tbl, ok := v.(map[string]any)
	if !ok {
		return Entry{}, false, fmt.Errorf("[needs.%q] is not a table", base)
	}
	bad := func(key string, err error) (Entry, bool, error) {
		return Entry{}, false, fmt.Errorf("[needs.%q] %s: %v", base, key, err)
	}
	var e Entry
	if v, ok := tbl["cpu"]; ok {
		s, err := scalar(v)
		if err != nil {
			return bad("cpu", err)
		}
		_, r, err := spec.ParseNeed("cpu=" + s)
		if err != nil {
			return bad("cpu", err)
		}
		e.CPU = &r
	}
	if v, ok := tbl["mem"]; ok {
		s, err := scalar(v)
		if err != nil {
			return bad("mem", err)
		}
		mib, err := spec.ParseBytes(s)
		if err == nil && mib == 0 {
			err = errors.New("mem needs more than 0")
		}
		if err != nil {
			return bad("mem", err)
		}
		e.MemMiB = mib
	}
	if v, ok := tbl["pinned"]; ok {
		b, ok := v.(bool)
		if !ok {
			return bad("pinned", errors.New("want a boolean"))
		}
		e.Pinned = b
	}
	if v, ok := tbl["cpu_per_worker"]; ok {
		switch v.(type) {
		case int64, float64:
			e.PerWorker = true
		default:
			return bad("cpu_per_worker", errors.New("want a number"))
		}
	}
	return e, true, nil
}

// scalar is a value written as a string or a TOML integer, as text.
func scalar(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	}
	return "", fmt.Errorf("want a string or an integer, not %v", v)
}
