//go:build unix

package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// loadWithin runs Load(path) and fails the test when it has not returned after a short bound.
// A Load blocked in open(2) on a FIFO is released at cleanup so that no goroutine outlives the test.
func loadWithin(t *testing.T, path string) (Config, error) {
	t.Helper()
	type result struct {
		c   Config
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := Load(path)
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		return r.c, r.err
	case <-time.After(3 * time.Second):
		if f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil { // lets a blocked reader's open return
			t.Cleanup(func() { f.Close() })
		}
		t.Fatalf("Load(%s) did not return within 3 s: it blocks on a non-regular file", path)
		return Config{}, nil
	}
}

func wantConfigError(t *testing.T, err error, what string) {
	t.Helper()
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("%s: got %v (%T), want a *config.Error", what, err, err)
	}
}

func TestLoadFIFOIsInvalidAndDoesNotBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadWithin(t, path)
	wantConfigError(t, err, "a FIFO at the configuration path")
}

func TestLoadFIFOBehindASymlinkIsInvalidAndDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.Symlink(fifo, path); err != nil {
		t.Fatal(err)
	}
	_, err := loadWithin(t, path)
	wantConfigError(t, err, "a symlink to a FIFO at the configuration path")
}

func TestLoadDirectoryIsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := loadWithin(t, path)
	wantConfigError(t, err, "a directory at the configuration path")
}

func TestLoadDanglingSymlinkIsAMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), path); err != nil {
		t.Fatal(err)
	}
	c, err := loadWithin(t, path)
	if err != nil {
		t.Fatalf("a dangling symlink must read as a missing file: %v", err)
	}
	if got := c.Sizes(Machine{CPUs: 8})["cpu"]; got != 11 || len(c.Resources) != 2 {
		t.Errorf("a dangling symlink did not give the defaults: cpu %d, resources %v", got, c.Resources)
	}
}

func TestLoadReadsThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.toml")
	if err := os.WriteFile(target, []byte("[resources]\ncpu = 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	c, err := loadWithin(t, path)
	if err != nil || c.Sizes(Machine{CPUs: 8})["cpu"] != 3 {
		t.Errorf("Load through a symlink = cpu %d, %v; want 3", c.Sizes(Machine{CPUs: 8})["cpu"], err)
	}
}
