//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// startIn runs startCommand in this process against dir, so that a test can shorten its wait.
func startIn(t *testing.T, dir string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv("CORESLOT_STATE_DIR", dir)
	stdout, stderr = capture(t, func(io IO) { code = startCommand(args, io) })
	return code, stdout, stderr
}

// pollUntilEnd stands in for tk's run: it polls the ticket every 20 ms until it is admitted or the
// test ends, and closes the grant then.
func pollUntilEnd(t *testing.T, tk *engine.Ticket) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if r, err := tk.Poll(); err != nil {
				return
			} else if a, ok := r.(engine.Admitted); ok {
				<-ctx.Done()
				a.Close()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

// ticketIDOf is the id `status` prints for tk.
func ticketIDOf(t *testing.T, tk *engine.Ticket) string {
	t.Helper()
	id, ok := wire.ParseName(tk.Name())
	if !ok {
		t.Fatalf("ticket name %q", tk.Name())
	}
	return id.String()
}

// start starts a waiting ticket now, ahead of the head and past the room, and says so; status
// marks its hold forced, and the head keeps waiting. --no-wait returns at once with the request
// recorded in start_requests.
func TestStartCommand(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	hold(t, c, "gate", 512)
	head := queue(t, c, "head", 256)
	tk := queue(t, c, "late", 384)
	pollUntilEnd(t, tk)

	code, stdout, stderr := startIn(t, dir, ticketIDOf(t, tk))
	if want := "coreslot: late started now, out of turn; its full need (384 MiB) still counts\n"; code != 0 || stdout != want {
		t.Fatalf("start: exit %d, stdout %q (stderr %q); want 0 and %q", code, stdout, stderr, want)
	}
	doc := statusDoc(t, dir, 0)
	forced := 0
	for _, h := range doc["holds"].([]any) {
		if m := h.(map[string]any); m["forced"] == true && m["label"] == "late" && m["need_mib"] == 384.0 {
			forced++
		}
	}
	if forced != 1 {
		t.Errorf("status lists %d forced holds of late, want 1: %v", forced, doc["holds"])
	}
	if room := doc["mem"].(map[string]any)["room_mib"].(float64); room >= 0 {
		t.Errorf("room_mib %v after a forced start past it, want below 0", room)
	}
	if r := cli(t, baseEnv(dir), "status"); !strings.Contains(r.stdout, "started now, out of turn") {
		t.Errorf("status does not mark the forced hold:\n%s", r.stdout)
	}
	if r, err := head.Poll(); err != nil || admitted(r) {
		t.Errorf("the head: admitted %v, %v; want it still waiting", admitted(r), err)
	}

	other := queue(t, c, "other", 128) // nobody polls it
	code, stdout, _ = startIn(t, dir, "--label", "other", "--no-wait")
	if code != 0 || !strings.Contains(stdout, "start requested for ticket "+ticketIDOf(t, other)) {
		t.Errorf("start --no-wait: exit %d, stdout %q", code, stdout)
	}
	starts := statusDoc(t, dir, 0)["start_requests"].([]any)
	if len(starts) != 1 || starts[0].(map[string]any)["ticket"] != ticketIDOf(t, other) || starts[0].(map[string]any)["unanswered"] != false {
		t.Errorf("start_requests %v; want the fresh request for %s, answered", starts, ticketIDOf(t, other))
	}
}

// A start request older than 10 s whose ticket still waits is unanswered: its run is stopped, or an
// older coreslot started it. Mutation: never setting it leaves the old request answered.
func TestStartRequestTurnsUnanswered(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	hold(t, c, "gate", 512)
	old, fresh := queue(t, c, "old", 128), queue(t, c, "fresh", 128)
	if err := os.MkdirAll(filepath.Join(dir, store.StartDir), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct {
		tk  *engine.Ticket
		age time.Duration
	}{{old, 11 * time.Second}, {fresh, time.Second}} {
		body, err := wire.Encode(wire.Cancel{RequestedNS: time.Now().Add(-w.age).UnixNano(), RequesterPID: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, store.StartDir, ticketIDOf(t, w.tk)), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{}
	for _, s := range statusDoc(t, dir, 0)["start_requests"].([]any) {
		m := s.(map[string]any)
		got[m["ticket"].(string)] = m["unanswered"].(bool)
	}
	if len(got) != 2 || !got[ticketIDOf(t, old)] || got[ticketIDOf(t, fresh)] {
		t.Errorf("unanswered by ticket %v; want only the 11 s old one", got)
	}
	if r := cli(t, baseEnv(dir), "status"); !strings.Contains(r.stdout, "pending start requests: 2, 1 unanswered") {
		t.Errorf("status does not count the unanswered request:\n%s", r.stdout)
	}
}

// An old start request is unanswered only while its ticket still waits: one whose ticket is gone or
// dead is answered or moot. Mutation: dropping the ticket check marks the last two unanswered.
func TestStartRequestUnansweredOnlyWhileItsTicketWaits(t *testing.T) {
	now := time.Now().UnixNano()
	old := now - int64(11*time.Second)
	r := engine.Report{
		Tickets: []engine.TicketInfo{{ID: "waits"}, {ID: "dead", Dead: true}},
		Starts:  []engine.StartInfo{{Ticket: "waits", RequestedNS: old}, {Ticket: "gone", RequestedNS: old}, {Ticket: "dead", RequestedNS: old}},
	}
	got := map[string]bool{}
	for _, s := range startRequests(r, now) {
		got[s.Ticket] = s.Unanswered
	}
	if !got["waits"] || got["gone"] || got["dead"] {
		t.Errorf("unanswered by ticket %v; want only the one whose ticket waits", got)
	}
}

// start refuses what cancel refuses: a label two waiting tickets share, a ticket that is not
// waiting; and takes exactly one ticket. Nothing is published on a refusal.
func TestStartRefusals(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	hold(t, c, "gate", 512)
	tk := queue(t, c, "twin", 128)
	queue(t, c, "twin", 128)
	if r := cli(t, baseEnv(dir), "start", "--label", "twin"); r.code != 75 || !strings.Contains(r.stderr, "2 waiting tickets have the label twin") {
		t.Errorf("start --label twin: exit %d, want 75 naming the two (stderr %q)", r.code, r.stderr)
	}
	gone := wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String()
	if r := cli(t, baseEnv(dir), "start", gone); r.code != 75 || !strings.Contains(r.stderr, "no waiting ticket") {
		t.Errorf("start of an unknown ticket: exit %d, want 75 (stderr %q)", r.code, r.stderr)
	}
	if got := entries(t, dir, store.StartDir); len(got) != 0 {
		t.Errorf("a refusal published start requests: %v", got)
	}
	id := ticketIDOf(t, tk)
	for _, args := range [][]string{{"start"}, {"start", "not-an-id"}, {"start", id, "--label", "x"}} {
		if r := cli(t, baseEnv(dir), args...); r.code != 64 {
			t.Errorf("%v: exit %d, want 64 (stderr %q)", args, r.code, r.stderr)
		}
	}
}
