//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// markIn runs quiet (or no-stop with noStop) in this process against dir.
func markIn(t *testing.T, dir string, noStop bool, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv("CORESLOT_STATE_DIR", dir)
	f := quietCommand
	if noStop {
		f = noStopCommand
	}
	stdout, stderr = capture(t, func(io IO) { code = f(args, io) })
	return code, stdout, stderr
}

func marked(t *testing.T, dir, kind, id string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(dir, kind, id))
	return err == nil
}

// quiet and no-stop mark a waiting ticket and a running hold by the id status shows; a second mark
// says so; --off, before or after the id, removes it. An id that names no live ticket or hold, and
// an --off with nothing to remove, are refusals (exit 75); a malformed id is a usage error.
func TestMarkCommands(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	g := hold(t, c, "gate", 512)
	tk := queue(t, c, "late", 128)
	for _, noStop := range []bool{false, true} {
		kind := store.QuietDir
		if noStop {
			kind = store.NoStopDir
		}
		for _, id := range []string{ticketIDOf(t, tk), holdIDOf(t, g)} {
			if code, stdout, stderr := markIn(t, dir, noStop, id); code != 0 || !marked(t, dir, kind, id) {
				t.Fatalf("%s %s: exit %d, stdout %q, stderr %q, marked %v", kind, id, code, stdout, stderr, marked(t, dir, kind, id))
			}
			if code, stdout, _ := markIn(t, dir, noStop, id); code != 0 || !strings.Contains(stdout, "already marked") {
				t.Errorf("%s %s again: exit %d, stdout %q", kind, id, code, stdout)
			}
		}
		if code, _, _ := markIn(t, dir, noStop, "--off", ticketIDOf(t, tk)); code != 0 || marked(t, dir, kind, ticketIDOf(t, tk)) {
			t.Errorf("%s --off <ticket>: exit %d, still marked %v", kind, code, marked(t, dir, kind, ticketIDOf(t, tk)))
		}
		if code, _, _ := markIn(t, dir, noStop, holdIDOf(t, g), "--off"); code != 0 || marked(t, dir, kind, holdIDOf(t, g)) {
			t.Errorf("%s <hold> --off: exit %d, still marked %v", kind, code, marked(t, dir, kind, holdIDOf(t, g)))
		}
		if code, _, stderr := markIn(t, dir, noStop, "--off", holdIDOf(t, g)); code != exitRefusal || !strings.Contains(stderr, "no such mark") {
			t.Errorf("%s --off with nothing marked: exit %d, stderr %q; want 75", kind, code, stderr)
		}
	}
	unknown := wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String()
	if code, _, stderr := markIn(t, dir, false, unknown); code != exitRefusal || !strings.Contains(stderr, "no live ticket or hold") {
		t.Errorf("quiet <unknown>: exit %d, stderr %q; want 75", code, stderr)
	}
	if marked(t, dir, store.QuietDir, unknown) {
		t.Error("an unknown id was marked")
	}
	if code, _, _ := markIn(t, dir, true, "not-an-id"); code != exitUsage {
		t.Errorf("no-stop not-an-id: exit %d, want 64", code)
	}
}

// --off with the id of a ticket that has meanwhile been admitted removes the mark of its run's hold:
// the same pid, its arrival the ticket's order key, two such holds after a restart. Mutation: look
// only for the given id → exit 75 and the marks stay.
func TestOffFollowsTicketToHolds(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	tk := queue(t, c, "job", 128)
	ticket := ticketIDOf(t, tk)
	r, err := tk.Poll()
	if err != nil || !admitted(r) {
		t.Fatalf("not admitted: %v, %v", r, err)
	}
	g := r.(engine.Admitted).Grant
	defer g.Close()
	if code, _, stderr := markIn(t, dir, true, holdIDOf(t, g)); code != 0 {
		t.Fatalf("no-stop <hold>: exit %d, %s", code, stderr)
	}
	if code, stdout, stderr := markIn(t, dir, true, "--off", ticket); code != 0 || marked(t, dir, store.NoStopDir, holdIDOf(t, g)) {
		t.Errorf("no-stop --off <admitted ticket>: exit %d, stdout %q, stderr %q; the hold still marked %v",
			code, stdout, stderr, marked(t, dir, store.NoStopDir, holdIDOf(t, g)))
	}

	// After a restart: two live holds of the run, the first kept by a descendant.
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	arrival := int64(1791720000000000000)
	var holdIDs []string
	for i := range 2 {
		f, name, err := sec.PublishHold(wire.Hold{Label: "job", PID: os.Getpid(), StartNS: arrival + int64(i+1)*1e9, ArrivalNS: arrival, NeedMiB: 64}, 900+i)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		id, _ := wire.ParseName(name)
		holdIDs = append(holdIDs, id.String())
		if _, err := sec.Mark(store.QuietDir, id); err != nil {
			t.Fatal(err)
		}
	}
	sec.Leave()
	first := wire.ID{OrderKey: wire.OrderKey(arrival), PID: os.Getpid(), Seq: 7}.String()
	if code, _, stderr := markIn(t, dir, false, "--off", first); code != 0 {
		t.Errorf("quiet --off <first ticket>: exit %d, stderr %q; want 0", code, stderr)
	}
	for _, id := range holdIDs {
		if marked(t, dir, store.QuietDir, id) {
			t.Errorf("hold %s is still marked quiet", id)
		}
	}
}
