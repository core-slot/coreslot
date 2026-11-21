// Package needs reads the needs file: one per machine, beside the configuration, with what a
// labelled run needs when its --mem leaves it out.
package needs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/BurntSushi/toml"

	"github.com/core-slot/coreslot/internal/spec"
)

// PathFor is the needs file that goes with the configuration file cfgPath.
func PathFor(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "needs.toml")
}

// Entry is what the needs file says about one label. MemMiB and DockerMemMiB are 0 when the entry
// leaves them out.
type Entry struct {
	MemMiB        int64
	DockerMemMiB  int64 // docker_mem: the run's Docker need, what its containers take of Docker's VM
	NoRestart     bool  // the run is never stopped to free memory
	DockerForward *bool // docker_forward: a forwarder always (true) or never (false); nil when absent
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
	v, ok := raw["needs"]
	if !ok {
		return File{}, nil
	}
	tbl, ok := v.(map[string]any)
	if !ok {
		return File{}, errors.New("[needs] is not a table")
	}
	return File{entries: tbl}, nil
}

// Lookup returns the entry for label. An empty label finds nothing. mem and docker_mem are sizes
// above 0, a string or an integer of GiB. Unknown keys are ignored; a known key with a bad value is an error naming the
// label and the key.
func (f File) Lookup(label string) (Entry, bool, error) {
	if label == "" {
		return Entry{}, false, nil
	}
	v, ok := f.entries[label]
	if !ok {
		return Entry{}, false, nil
	}
	tbl, ok := v.(map[string]any)
	if !ok {
		return Entry{}, false, fmt.Errorf("[needs.%q] is not a table", label)
	}
	bad := func(key string, err error) (Entry, bool, error) {
		return Entry{}, false, fmt.Errorf("[needs.%q] %s: %v", label, key, err)
	}
	var e Entry
	for _, k := range []struct {
		key string
		dst *int64
	}{{"mem", &e.MemMiB}, {"docker_mem", &e.DockerMemMiB}} {
		if v, ok := tbl[k.key]; ok {
			mib, err := size(v)
			if err != nil {
				return bad(k.key, err)
			}
			*k.dst = mib
		}
	}
	if v, ok := tbl["no_restart"]; ok {
		b, ok := v.(bool)
		if !ok {
			return bad("no_restart", errors.New("want a boolean"))
		}
		e.NoRestart = b
	}
	if v, ok := tbl["docker_forward"]; ok {
		b, ok := v.(bool)
		if !ok {
			return bad("docker_forward", errors.New("want a boolean"))
		}
		e.DockerForward = &b
	}
	return e, true, nil
}

// size is a value written as a size above 0, in MiB.
func size(v any) (int64, error) {
	s, err := scalar(v)
	if err != nil {
		return 0, err
	}
	mib, err := spec.ParseBytes(s)
	if err == nil && mib == 0 {
		err = errors.New("needs more than 0")
	}
	return mib, err
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
