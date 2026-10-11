package config

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"sync"
	"time"
)

// Watcher returns the configuration at Path, reloading it when the file's mtime (or size, or
// existence) changes. The zero value with Path set is ready to use; it is safe for concurrent use.
type Watcher struct {
	Path string

	mu     sync.Mutex
	loaded bool
	exists bool
	mtime  time.Time
	size   int64
	cfg    Config
	err    error
}

// Current returns the configuration, reloading it first if the file changed since the last call.
// A load error is returned until the file changes again.
func (w *Watcher) Current() (Config, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Stat before loading: a change that lands during the load shows as a newer mtime next time.
	fi, err := os.Stat(w.Path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		w.loaded = false
		return Load(w.Path)
	}
	exists := err == nil
	var mtime time.Time
	var size int64
	if exists {
		mtime, size = fi.ModTime(), fi.Size()
	}
	if !w.loaded || exists != w.exists || !mtime.Equal(w.mtime) || size != w.size {
		w.cfg, w.err = Load(w.Path)
		w.loaded, w.exists, w.mtime, w.size = true, exists, mtime, size
	}
	if w.err != nil {
		return Config{}, w.err
	}
	c := w.cfg
	c.Resources = maps.Clone(c.Resources)
	c.Priorities = maps.Clone(c.Priorities)
	return c, nil
}
