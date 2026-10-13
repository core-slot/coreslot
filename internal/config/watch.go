package config

import "maps"

// Watcher returns the configuration at Path, read afresh at every call: sizes are recomputed at
// every admission attempt (PROTOCOL.md section 5.2), and no file attribute (mtime, size, inode,
// ctime) reliably tells a replaced configuration from the one read before, since a copy that
// keeps times (cp -p, rsync -t) or a rename can leave all of them unchanged. The file is small.
// The zero value with Path set is ready to use; it is safe for concurrent use.
type Watcher struct {
	Path string
}

// Current reads and returns the configuration; an invalid one is a *Error.
func (w *Watcher) Current() (Config, error) {
	c, err := Load(w.Path)
	if err != nil {
		return Config{}, err
	}
	c.Resources = maps.Clone(c.Resources)
	c.Priorities = maps.Clone(c.Priorities)
	return c, nil
}
