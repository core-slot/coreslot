//go:build unix

package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The override round-trips and is removed; with no file there is none. Mutation: SetWatchMode
// writing until_ns as 0 always reads back 0 in the second case.
func TestWatchModeRoundTrip(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	if _, _, ok, err := s.ReadWatchMode(); ok || err != nil {
		t.Fatalf("no file: ok %v, %v; want none", ok, err)
	}
	for _, c := range []struct {
		mode  string
		until int64
	}{{"observe", 0}, {"enforce", 1795300000000000000}} {
		sec := enter(t, s)
		if err := sec.SetWatchMode(c.mode, c.until); err != nil {
			t.Fatal(err)
		}
		sec.Leave()
		if mode, until, ok, err := s.ReadWatchMode(); !ok || err != nil || mode != c.mode || until != c.until {
			t.Errorf("read %q %d %v %v; want %q %d", mode, until, ok, err, c.mode, c.until)
		}
	}
	if fi, err := os.Lstat(filepath.Join(dir, WatchModeFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("watch-mode %v, %v; want a file of mode 0600", fi, err)
	}
	sec := enter(t, s)
	if err := sec.RemoveWatchMode(); err != nil {
		t.Fatal(err)
	}
	if err := sec.RemoveWatchMode(); err != nil {
		t.Errorf("removing a missing override: %v", err)
	}
	sec.Leave()
	if _, _, ok, _ := s.ReadWatchMode(); ok {
		t.Error("an override after RemoveWatchMode")
	}
	sec = enter(t, s)
	defer sec.Leave()
	if err := sec.SetWatchMode("off", 0); err == nil {
		t.Error("SetWatchMode accepted the mode off")
	}
}

// A body that is not an override is none, never an error. Mutations: accepting an unknown mode
// reads "off"; accepting a negative until_ns reads it; dropping the size check reads the padded
// file.
func TestReadWatchModeLenient(t *testing.T) {
	head := `{"mode":"observe","until_ns":0,"pad":"`
	big := head + strings.Repeat("x", maxWatchModeBytes+1-len(head)-2) + `"}` // whole JSON, one byte over
	for _, body := range []string{`{`, `[]`, `{"mode":"off"}`, `{"mode":"observe","until_ns":-1}`, big} {
		dir := newDir(t)
		s := open(t, dir)
		if err := os.WriteFile(filepath.Join(dir, WatchModeFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if mode, _, ok, err := s.ReadWatchMode(); ok || err != nil {
			t.Errorf("body %.40q: %q, ok %v, %v; want no override", body, mode, ok, err)
		}
	}
}

// watch-mode is checked on open as watch.json is: a symlink or a directory there is a state error.
// Mutation: leaving WatchModeFile out of openDir's list opens both.
func TestOpenChecksWatchModeFile(t *testing.T) {
	for _, c := range []struct {
		name string
		make func(path string) error
	}{
		{"symlink", func(p string) error { return os.Symlink(filepath.Join(filepath.Dir(p), AdmitLock), p) }},
		{"directory", func(p string) error { return os.Mkdir(p, 0o700) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := newDir(t)
			s := open(t, dir)
			if err := c.make(filepath.Join(dir, WatchModeFile)); err != nil {
				t.Fatal(err)
			}
			_, err := Open(dir)
			wantStateError(t, err, "Open with watch-mode a "+c.name)
			_, _, _, err = s.ReadWatchMode()
			wantStateError(t, err, "ReadWatchMode of a "+c.name)
		})
	}
}

// doctor --reset removes the override. Mutation: leaving WatchModeFile out of the reset's list
// keeps it.
func TestResetRemovesWatchMode(t *testing.T) {
	dir := newDir(t)
	_, sec := resetFixture(t, dir)
	if err := sec.SetWatchMode("observe", 0); err != nil {
		t.Fatal(err)
	}
	if live, done, err := sec.Reset(); err != nil || !live.Empty() {
		t.Fatalf("Reset = %+v, %+v, %v", live, done, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, WatchModeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("watch-mode after a reset: %v", err)
	}
}
