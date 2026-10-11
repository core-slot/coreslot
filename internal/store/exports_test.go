//go:build unix

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// CountLockedProbe counts every locked token at every index, ignoring non-token names, without
// entering the section: it works while another process holds the section.
func TestCountLockedProbeOutsideSection(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	var paths []string
	for i := 0; i < 6; i++ {
		paths = append(paths, tokenPath(dir, "cpu", i))
	}
	paths = append(paths, filepath.Join(dir, ResDir, "cpu", "007"), filepath.Join(dir, ResDir, "cpu", "4096"))
	if err := os.MkdirAll(filepath.Join(dir, ResDir, "cpu"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Lock tokens 2, 3 and 5 and both non-token names.
	startHelper(t, "lock", dir, strings.Join([]string{paths[2], paths[3], paths[5], paths[6], paths[7]}, "\n"), nil)
	startHelper(t, "section", dir, "", nil)
	n, err := s.CountLockedProbe("cpu")
	if err != nil || n != 3 {
		t.Fatalf("CountLockedProbe = %d, %v; want 3 (007 and 4096 are not tokens)", n, err)
	}
	if n, err := s.CountLockedProbe("gpu"); err != nil || n != 0 {
		t.Errorf("a resource without res/<name>/: %d, %v; want 0", n, err)
	}
}

func TestReadFormatOutsideSection(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	if n, present, err := s.ReadFormat(); err != nil || present || n != 0 {
		t.Fatalf("absent FORMAT: %d %v %v", n, present, err)
	}
	if exists(filepath.Join(dir, FormatFile)) {
		t.Fatal("ReadFormat wrote FORMAT")
	}
	for body, want := range map[string]int{"coreslot-protocol 1\n": 1, "coreslot-protocol 01": 1, "coreslot-protocol 2\n": 2} {
		if err := os.WriteFile(filepath.Join(dir, FormatFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if n, present, err := s.ReadFormat(); err != nil || !present || n != want {
			t.Errorf("FORMAT %q: %d %v %v, want %d", body, n, present, err, want)
		}
	}
	for _, body := range []string{"coreslot-protocol x\n", "coreslot-protocol 1\r\n", "coreslot-protocol 1234567890"} {
		if err := os.WriteFile(filepath.Join(dir, FormatFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := s.ReadFormat()
		wantStateError(t, err, "FORMAT "+body)
	}
}

// RemoveOwnTicket unlinks only a ticket nobody locks; a missing or stray one is left alone.
func TestRemoveOwnTicket(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	q := filepath.Join(dir, QueueDir)
	if err := os.Mkdir(q, 0o700); err != nil {
		t.Fatal(err)
	}
	live, dead, stray := ticketName(1), ticketName(2), ticketName(3)
	for _, n := range []string{live, dead} {
		if err := os.WriteFile(filepath.Join(q, n), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(q, dead), filepath.Join(q, stray)); err != nil {
		t.Fatal(err)
	}
	startHelper(t, "lock", dir, filepath.Join(q, live), nil)

	if removed, err := sec.RemoveOwnTicket(live); err != nil || removed || !exists(filepath.Join(q, live)) {
		t.Errorf("a locked ticket: removed %v, %v; want kept", removed, err)
	}
	if removed, err := sec.RemoveOwnTicket(stray); err != nil || removed || !exists(filepath.Join(q, stray)) {
		t.Errorf("a stray symlink: removed %v, %v; want left alone", removed, err)
	}
	if removed, err := sec.RemoveOwnTicket(dead); err != nil || !removed || exists(filepath.Join(q, dead)) {
		t.Errorf("an unlocked ticket: removed %v, %v; want removed", removed, err)
	}
	if removed, err := sec.RemoveOwnTicket(dead); err != nil || removed {
		t.Errorf("a missing ticket: removed %v, %v; want false, nil", removed, err)
	}
	if _, err := sec.RemoveOwnTicket(".tmp-0000000001-000001"); err == nil {
		t.Error("a temporary name: want an error")
	}
}

func TestRemoveCancel(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	c := filepath.Join(dir, CancelDir)
	if err := os.Mkdir(c, 0o700); err != nil {
		t.Fatal(err)
	}
	id := wire.ID{OrderKey: 1, PID: 2, Seq: 3}
	strayID := wire.ID{OrderKey: 1, PID: 2, Seq: 4}
	if err := os.WriteFile(filepath.Join(c, id.String()), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(c, strayID.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	if removed, err := sec.RemoveCancel(id); err != nil || !removed || exists(filepath.Join(c, id.String())) {
		t.Errorf("present: removed %v, %v", removed, err)
	}
	if removed, err := sec.RemoveCancel(id); err != nil || removed {
		t.Errorf("missing: removed %v, %v; want false, nil", removed, err)
	}
	if removed, err := sec.RemoveCancel(strayID); err != nil || removed || !exists(filepath.Join(c, strayID.String())) {
		t.Errorf("a stray directory: removed %v, %v; want left alone", removed, err)
	}
}
