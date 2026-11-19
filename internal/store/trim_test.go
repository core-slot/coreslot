//go:build unix

package store

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/wire"
)

// trimID is the id of the n-th hold of these tests.
func trimID(n int) wire.ID {
	return wire.ID{OrderKey: 1791720000000000000 + uint64(n), PID: 4711, Seq: 1}
}

func writeTrim(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, TrimFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTrim(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	got, err := s.ReadTrim()
	if err != nil {
		t.Fatalf("ReadTrim: %v", err)
	}
	return got
}

// A trim.json that is missing, too large or malformed is no request, a bad entry is skipped alone,
// and an entry is a request whatever it holds. Mutations: failing the whole file on one bad entry
// empties the "bad entries beside a good one" case; dropping the size check reads the file of
// 64 KiB + 1 bytes, whose JSON is whole; requiring an empty entry drops a's in that case.
func TestReadTrimLenient(t *testing.T) {
	a, b := trimID(1).String(), trimID(2).String()
	good := fmt.Sprintf(`{"holds":{%q:{}}}`, a)
	padTo := func(n int) string {
		head := fmt.Sprintf(`{"holds":{%q:{}},"pad":"`, a)
		body := head + strings.Repeat("x", n-len(head)-2) + `"}`
		if len(body) != n {
			t.Fatalf("padded body is %d bytes, want %d", len(body), n)
		}
		return body
	}
	want := map[string]bool{a: true}
	cases := []struct {
		name string
		body *string // nil: no file
		want map[string]bool
	}{
		{"missing", nil, map[string]bool{}},
		{"well-formed", &good, want},
		{"64 KiB", ptr(padTo(maxTrimBytes)), want},
		{"64 KiB + 1", ptr(padTo(maxTrimBytes + 1)), map[string]bool{}},
		{"not JSON", ptr(`{"holds":`), map[string]bool{}},
		{"not an object", ptr(`[1]`), map[string]bool{}},
		{"holds not an object", ptr(`{"holds":[1]}`), map[string]bool{}},
		{"bad entries beside a good one", ptr(fmt.Sprintf(`{"holds":{%q:{"later":[1]},`+
			`%q:7,"x":{},"%s-gate":{},%q:null,%q:[{}]},"unknown":{}}`,
			a, b, trimID(3), trimID(4), trimID(5))), want},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := newDir(t)
			s := open(t, dir)
			if c.body != nil {
				writeTrim(t, dir, *c.body)
			}
			if got := readTrim(t, s); !maps.Equal(got, c.want) {
				t.Errorf("ReadTrim = %v; want %v", got, c.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// A rewrite keeps the requests of holds still listed and adds the new one; a hold no longer listed
// loses its entry. Mutation: appending without pruning keeps b.
func TestRequestTrimKeepsListedDropsGone(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	a, b, c := trimID(1), trimID(2), trimID(3)
	writeTrim(t, dir, fmt.Sprintf(`{"holds":{%q:{},%q:{}}}`, a, b))
	sec := enter(t, s)
	if err := sec.RequestTrim(c, map[string]bool{a.String(): true, c.String(): true}); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{a.String(): true, c.String(): true}
	if got := readTrim(t, s); !maps.Equal(got, want) {
		t.Errorf("after the rewrite: %v; want %v", got, want)
	}
	if fi, err := os.Lstat(filepath.Join(dir, TrimFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("trim.json: %v, %v; want a file of mode 0600", fi, err)
	}
}

// A rewrite that would pass 64 KiB is refused with ErrTrimFull and leaves the file as it was, so
// that no reader ever finds it over the bound. Mutation: writing without the size check leaves a
// file the next ReadTrim reads as empty, dropping every request.
func TestRequestTrimFull(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	listed := map[string]bool{}
	var before []byte
	for n := 1; ; n++ {
		id := trimID(n)
		listed[id.String()] = true
		err := sec.RequestTrim(id, listed)
		if errors.Is(err, ErrTrimFull) {
			after, rerr := os.ReadFile(filepath.Join(dir, TrimFile))
			if rerr != nil || string(after) != string(before) || len(after) > maxTrimBytes {
				t.Fatalf("a refused rewrite changed trim.json (%d bytes, %v)", len(after), rerr)
			}
			if got := len(readTrim(t, s)); got != n-1 {
				t.Errorf("%d requests read back; want the %d written", got, n-1)
			}
			return
		}
		if err != nil {
			t.Fatalf("request %d: %v", n, err)
		}
		if n > 2000 {
			t.Fatal("2000 requests fit in 64 KiB")
		}
		before, _ = os.ReadFile(filepath.Join(dir, TrimFile))
	}
}

// A section that was left writes nothing. Mutation: writing trim.json in place with os.WriteFile
// instead of through replaceFile, which refuses a left section, writes the file.
func TestRequestTrimOutsideSectionFails(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	sec.Leave()
	if err := sec.RequestTrim(trimID(1), nil); !errors.Is(err, errLeft) {
		t.Errorf("RequestTrim after Leave: %v; want errLeft", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, TrimFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("trim.json written outside the section: %v", err)
	}
}

// trim.json is checked on open as watch.json is: a symlink or a directory there is a state error.
// Mutation: leaving TrimFile out of openDir's list opens both.
func TestOpenChecksTrimFile(t *testing.T) {
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
			if err := c.make(filepath.Join(dir, TrimFile)); err != nil {
				t.Fatal(err)
			}
			_, err := Open(dir)
			wantStateError(t, err, "Open with trim.json a "+c.name)
			_, err = OpenReadOnly(dir)
			wantStateError(t, err, "OpenReadOnly with trim.json a "+c.name)
			_, err = s.ReadTrim()
			wantStateError(t, err, "ReadTrim of a "+c.name)
		})
	}
}

// doctor --reset removes trim.json with gen and watch.json. Mutation: leaving TrimFile out of the
// reset's list keeps it.
func TestResetRemovesTrimFile(t *testing.T) {
	dir := newDir(t)
	_, sec := resetFixture(t, dir)
	if err := sec.RequestTrim(trimID(1), nil); err != nil {
		t.Fatal(err)
	}
	if live, done, err := sec.Reset(); err != nil || !live.Empty() || done.Removed != 8 {
		t.Fatalf("Reset = %+v, %+v, %v; want 8 entries removed, trim.json among them", live, done, err)
	}
	if exists(filepath.Join(dir, TrimFile)) {
		t.Error("trim.json survived the reset")
	}
}
