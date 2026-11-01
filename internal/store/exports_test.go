//go:build unix

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/wire"
)

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

// The owner's removal of its own ticket outlasts another client's momentary probe, which holds
// the ticket's lock at the owner's first tries: the owner tries again and unlinks it, rather than
// leave it dead in queue/ for a reconciliation (PROTOCOL.md section 12.1 step 7). A ticket that
// stays locked throughout is still kept, after a bounded number of tries and about a millisecond.
// The probe's busy tries are simulated through the flock seam, so the test does not depend on
// timing. Mutation: a single probe (ownRemovalTries = 1) leaves the first ticket in queue/.
func TestRemoveOwnTicketOutlastsAMomentaryProbe(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	q := filepath.Join(dir, QueueDir)
	if err := os.Mkdir(q, 0o700); err != nil {
		t.Fatal(err)
	}
	probed, kept := ticketName(1), ticketName(2)
	for _, n := range []string{probed, kept} {
		if err := os.WriteFile(filepath.Join(q, n), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tries := map[string]int{}
	old := flockNB
	t.Cleanup(func() { flockNB = old })
	flockNB = func(f *os.File) (bool, error) {
		name := filepath.Base(f.Name())
		tries[name]++
		if (name == probed && tries[name] <= 3) || name == kept {
			return false, nil // another description holds it at this try
		}
		return old(f)
	}

	if removed, err := sec.RemoveOwnTicket(probed); err != nil || !removed || exists(filepath.Join(q, probed)) {
		t.Errorf("a ticket probed by another client at the first 3 tries: removed %v, %v; want removed", removed, err)
	}
	if tries[probed] != 4 {
		t.Errorf("%d tries for a ticket free at the 4th, want 4", tries[probed])
	}
	start := time.Now()
	if removed, err := sec.RemoveOwnTicket(kept); err != nil || removed || !exists(filepath.Join(q, kept)) {
		t.Errorf("a ticket locked throughout: removed %v, %v; want kept", removed, err)
	}
	if took := time.Since(start); tries[kept] != ownRemovalTries || took > time.Second {
		t.Errorf("a ticket locked throughout: %d tries in %v; want %d, in about a millisecond", tries[kept], took, ownRemovalTries)
	}
}

// The owner's removal of its own hold after its command exited (PROTOCOL.md section 12.3)
// outlasts another client's momentary probe the same way. Mutation: ProbeAndRemoveHold calls
// removeIfDead once, and the hold probed at the first try stays in holds/.
func TestProbeAndRemoveHoldOutlastsAMomentaryProbe(t *testing.T) {
	dir := newDir(t)
	s := open(t, dir)
	sec := enter(t, s)
	h := filepath.Join(dir, HoldsDir)
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	probed := ticketName(1)
	if err := os.WriteFile(filepath.Join(h, probed), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	tries := 0
	old := flockNB
	t.Cleanup(func() { flockNB = old })
	flockNB = func(f *os.File) (bool, error) {
		if filepath.Base(f.Name()) == probed {
			if tries++; tries == 1 {
				return false, nil // another description holds it at this try
			}
		}
		return old(f)
	}
	if removed, err := sec.ProbeAndRemoveHold(probed); err != nil || !removed || exists(filepath.Join(h, probed)) {
		t.Errorf("a hold probed by another client at the first try: removed %v, %v; want removed", removed, err)
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
