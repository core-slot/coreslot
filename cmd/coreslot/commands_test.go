//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// ---------------------------------------------------------------- fixtures

// openLib opens an engine over dir, as the test's own client: what it holds and enqueues is
// held by the test process, so the CLI the test runs sees live holds and tickets.
func openLib(t *testing.T, dir string) *engine.Engine {
	t.Helper()
	if mib, ok := budgetRoom.Load(dir); ok {
		saved := readMachine
		readMachine = machineWithHeadroom(mib.(int64))
		t.Cleanup(func() { readMachine = saved })
	}
	e, err := openEngine(location{dir: dir, cfgPath: filepath.Join(dir, store.ConfigFile)}, false)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// recordDegraded writes a degraded record into st as a fail-open run does: inside the section,
// after FORMAT and a due reconciliation.
func recordDegraded(t *testing.T, st *store.Store, reason string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sec, err := st.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if _, understood, err := sec.Format(); err != nil || !understood {
		t.Fatalf("FORMAT before the degraded record: understood %v, %v", understood, err)
	}
	if _, err := sec.ReconcileIfDue(store.NowNS()); err != nil {
		t.Fatal(err)
	}
	if err := sec.RecordDegraded(reason); err != nil {
		t.Fatal(err)
	}
}

// hold admits a request for the label at once, which the budget must have room for, and keeps
// the grant until the test ends.
func hold(t *testing.T, c *engine.Engine, label string, units int) *engine.Grant {
	t.Helper()
	r, err := queue(t, c, label, units).Poll()
	if err != nil || !admitted(r) {
		t.Fatalf("hold %s: not admitted at once (%v, %v)", label, r, err)
	}
	g := r.(engine.Admitted).Grant
	t.Cleanup(func() { g.Close() })
	return g
}

func admitted(r engine.StepResult) bool {
	_, ok := r.(engine.Admitted)
	return ok
}

// queue enqueues a request for units of 128 MiB and withdraws it when the test ends.
func queue(t *testing.T, c *engine.Engine, label string, units int) *engine.Ticket {
	t.Helper()
	w, err := c.EnqueueContext(context.Background(), spec.Request{Label: label, NeedMiB: int64(units) * 128})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Withdraw() })
	return w
}

// statusDoc runs `status --json` and decodes it.
func statusDoc(t *testing.T, dir string, wantCode int) map[string]any {
	t.Helper()
	r := cli(t, baseEnv(dir), "status", "--json")
	if r.code != wantCode {
		t.Fatalf("status --json exited %d, want %d (stderr %q)", r.code, wantCode, r.stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("status --json: %v\n%s", err, r.stdout)
	}
	return doc
}

func keysOf(m map[string]any) string {
	return strings.Join(slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}), " ")
}

// tree is every path under dir with its mode and size, for "nothing changed" assertions.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, fmt.Sprintf("%s %v %d", rel, fi.Mode(), fi.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// treeContent is every path under dir with its mode, size and inode, and the bytes of every
// regular file and the target of every symlink: equal strings, equal trees. A refused reset must
// not rewrite admit.lock, and a size comparison alone passes or fails by the digit count of a
// pid (macOS wraps them at 99999).
func treeContent(t *testing.T, dir string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(&out, "%s %v size=%d ino=%d", rel, fi.Mode(), fi.Size(), fi.Sys().(*syscall.Stat_t).Ino)
		switch {
		case fi.Mode().IsRegular():
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&out, " body=%q", body)
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&out, " -> %s", target)
		}
		out.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// ---------------------------------------------------------------- status

func TestStatusJSONSchema(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	hold(t, c, "gate", 3)
	queue(t, c, "waiting", 2)

	doc := statusDoc(t, dir, 0)
	const top = "cancel_requests config_error dead_holds degraded format format_error format_present format_understood gen holds ignored mem observed_s reconciled_ns schema section state_dir state_dir_exists tickets watch"
	if got := keysOf(doc); got != top {
		t.Errorf("top-level keys\n got %s\nwant %s", got, top)
	}
	if doc["schema"] != float64(1) {
		t.Errorf("schema = %v, want 1", doc["schema"])
	}
	if doc["degraded"] != nil {
		t.Errorf("degraded = %v, want null", doc["degraded"])
	}
	if got := keysOf(doc["section"].(map[string]any)); got != "held holder" {
		t.Errorf("section keys %s", got)
	}
	if sec := doc["section"].(map[string]any); sec["held"] != false || sec["holder"] != nil {
		t.Errorf("a free section: %v, want held false and a null holder", sec)
	}
	if doc["state_dir_exists"] != true {
		t.Errorf("state_dir_exists = %v", doc["state_dir_exists"])
	}

	mem := doc["mem"].(map[string]any)
	if got := keysOf(mem); got != "free_mib reserved_mib size_mib" {
		t.Errorf("mem keys %s", got)
	}
	if mem["size_mib"] != float64(512) || mem["reserved_mib"] != float64(384) || mem["free_mib"] != float64(128) {
		t.Errorf("mem = %v, want size 512, reserved 384, free 128", mem)
	}

	holds := doc["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v", holds)
	}
	h := holds[0].(map[string]any)
	const holdKeys = "arrival_ns footprint_mib held_for_s label name need_mib owner_exited pid start_ns stops"
	if got := keysOf(h); got != holdKeys {
		t.Errorf("hold keys\n got %s\nwant %s", got, holdKeys)
	}
	if h["label"] != "gate" || h["owner_exited"] != false || h["need_mib"] != float64(384) || h["pid"] != float64(os.Getpid()) {
		t.Errorf("hold = %v", h)
	}

	tickets := doc["tickets"].([]any)
	if len(tickets) != 1 {
		t.Fatalf("tickets = %v", tickets)
	}
	tk := tickets[0].(map[string]any)
	const ticketKeys = "created_ns id label need_mib pid position stalled state waited_s"
	if got := keysOf(tk); got != ticketKeys {
		t.Errorf("ticket keys\n got %s\nwant %s", got, ticketKeys)
	}
	if tk["label"] != "waiting" || tk["position"] != float64(1) || tk["state"] != "waiting" {
		t.Errorf("ticket = %v", tk)
	}
	if v, present := tk["stalled"]; !present || v != nil {
		t.Errorf("stalled = %v (present %v), want null without --observe", v, present)
	}
	if tk["need_mib"] != float64(256) {
		t.Errorf("need_mib = %v, want 256", tk["need_mib"])
	}
	if _, ok := wire.ParseID(tk["id"].(string)); !ok {
		t.Errorf("ticket id %q is not an id", tk["id"])
	}
	for _, k := range []string{"queue", "holds", "cancel"} {
		if _, ok := doc["ignored"].(map[string]any)[k]; !ok {
			t.Errorf("ignored lacks %q: %v", k, doc["ignored"])
		}
	}
}

// With --observe the JSON says, for each ticket, whether it stalled: true for one nobody
// refreshes, false for one a waiter keeps polling. The human output says when it did not look.
func TestStatusObserveReportsStalled(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	hold(t, c, "gate", 4)
	queue(t, c, "idle", 1) // never polled: nobody refreshes it
	live := queue(t, c, "polled", 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond):
				_, _ = live.Poll()
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })

	plain := cli(t, baseEnv(dir), "status")
	if !strings.Contains(plain.stdout, "STALLED: not observed (use --observe 10s or more)") {
		t.Errorf("status without --observe does not say STALLED was not observed:\n%s", plain.stdout)
	}
	short := cli(t, baseEnv(dir), "status", "--json", "--observe", "1s")
	if short.code != 0 || !strings.Contains(short.stderr, "shorter than 10s") {
		t.Errorf("--observe 1s: exit %d, want a warning about 10s (stderr %q)", short.code, short.stderr)
	}

	r := cli(t, baseEnv(dir), "status", "--json", "--observe", "10s")
	if r.code != 0 {
		t.Fatalf("status --observe 10s exited %d (stderr %q)", r.code, r.stderr)
	}
	if r.stderr != "" {
		t.Errorf("--observe 10s printed %q, want no warning", r.stderr)
	}
	var doc struct {
		ObservedS float64 `json:"observed_s"`
		Tickets   []struct {
			Label   string `json:"label"`
			Stalled *bool  `json:"stalled"`
			State   string `json:"state"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tk := range doc.Tickets {
		if tk.Stalled == nil {
			t.Fatalf("ticket %s: stalled is null after --observe", tk.Label)
		}
		got[tk.Label] = fmt.Sprintf("%v/%s", *tk.Stalled, tk.State)
	}
	if got["idle"] != "true/stalled" || got["polled"] != "false/waiting" || doc.ObservedS != 10 {
		t.Errorf("tickets %v, observed %v s; want idle true/stalled, polled false/waiting, 10 s", got, doc.ObservedS)
	}
}

// status shows each waiting ticket's need on the ticket's line as the waiting line writes it,
// between its pid and how long it has waited, and the budget's line.
func TestStatusTicketLineShowsTheNeed(t *testing.T) {
	dir := budgetDir(t, 2048)
	c := openLib(t, dir)
	hold(t, c, "gate", 16)
	queue(t, c, "wide", 12)
	r := cli(t, baseEnv(dir), "status")
	want := fmt.Sprintf("\n  1. wide  pid %d  need mem 1.5 GiB  waiting ", os.Getpid())
	if r.code != 0 || !strings.Contains(r.stdout, want) {
		t.Errorf("status: exit %d, want a ticket line with %q:\n%s", r.code, want, r.stdout)
	}
	if line := "\nmem: size 2 GiB  reserved 2 GiB  free 0 MiB\n"; !strings.Contains(r.stdout, line) {
		t.Errorf("status lacks the budget line %q:\n%s", strings.TrimSpace(line), r.stdout)
	}
}

// memReport is a snapshot with a 9.5 GiB mem budget, 3 GiB of it reserved by one hold.
func memReport() engine.Report {
	return engine.Report{Mem: engine.MemInfo{Known: true, SizeMiB: 9728, ReservedMiB: 3072, FreeMiB: 6656},
		Holds: []engine.HolderInfo{{Name: "h", Label: "gate", PID: 7, NeedMiB: 3072}}}
}

// status prints mem in bytes and each hold's need, `unbudgeted` for one that reserves nothing.
func TestStatusHumanMem(t *testing.T) {
	rep := memReport()
	rep.Holds = append(rep.Holds, engine.HolderInfo{Name: "u", Label: "open", PID: 8})
	h := buildStatus(rep, 0, nil, 0).human()
	for _, line := range []string{"\nmem: size 9.5 GiB  reserved 3 GiB  free 6.5 GiB\n",
		"  gate  pid 7  mem 3 GiB  held ", "  open  pid 8  unbudgeted  held "} {
		if !strings.Contains(h, line) {
			t.Errorf("status lacks the line %q:\n%s", strings.TrimSpace(line), h)
		}
	}
}

// status --json carries the budget and each hold's need in MiB.
func TestStatusJSONMemMiB(t *testing.T) {
	var buf bytes.Buffer
	if err := buildStatus(memReport(), 0, nil, 0).writeJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Schema int              `json:"schema"`
		Mem    map[string]int64 `json:"mem"`
		Holds  []struct {
			NeedMiB int64 `json:"need_mib"`
		} `json:"holds"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	wantMem := map[string]int64{"size_mib": 9728, "reserved_mib": 3072, "free_mib": 6656}
	if doc.Schema != 1 || !maps.Equal(doc.Mem, wantMem) || len(doc.Holds) != 1 || doc.Holds[0].NeedMiB != 3072 {
		t.Errorf("schema %d, mem %v, holds %+v; want schema 1, mem %v and one hold of 3072", doc.Schema, doc.Mem, doc.Holds, wantMem)
	}
}

func TestStatusEscapesLabels(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	labels := []string{
		"line one\nline two",
		"\x1b[31mred\x1b[0m",
		"tab\there\rcarriage",
		"rtl\u202eoverride",
		`back\slash "quote" <tag>`,
		"c1\u009bcontrol",
	}
	hold(t, c, labels[0], 4)
	for _, l := range labels[1:] {
		queue(t, c, l, 1)
	}

	r := cli(t, baseEnv(dir), "status")
	if r.code != 0 {
		t.Fatalf("status exited %d (stderr %q)", r.code, r.stderr)
	}
	for i := 0; i < len(r.stdout); i++ {
		if b := r.stdout[i]; b < 0x20 && b != '\n' || b == 0x7f {
			t.Fatalf("status printed control byte %#x:\n%q", b, r.stdout)
		}
	}
	for _, c := range r.stdout {
		if c >= 0x80 && c <= 0x9f {
			t.Fatalf("status printed C1 control %U:\n%q", c, r.stdout)
		}
	}
	if strings.ContainsRune(r.stdout, '\u202e') {
		t.Errorf("status printed the bidi override:\n%q", r.stdout)
	}
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	for _, l := range labels {
		esc := wire.EscapeHuman(l)
		n := 0
		for _, line := range lines {
			if strings.Contains(line, esc) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("label %q (as %s) is on %d lines, want exactly one:\n%s", l, esc, n, r.stdout)
		}
	}
	entries := 0
	for _, line := range lines {
		if strings.Contains(line, " pid ") {
			entries++
		}
	}
	if entries != len(labels) {
		t.Errorf("%d lines carry an entry, want %d (one per hold and ticket):\n%s", entries, len(labels), r.stdout)
	}

	doc := statusDoc(t, dir, 0)
	var got []string
	for _, h := range doc["holds"].([]any) {
		got = append(got, h.(map[string]any)["label"].(string))
	}
	for _, tk := range doc["tickets"].([]any) {
		got = append(got, tk.(map[string]any)["label"].(string))
	}
	if !slices.Equal(got, labels) {
		t.Errorf("JSON labels\n got %q\nwant %q", got, labels)
	}
}

// rawTerminalControls lists the characters in b that act on a terminal when printed raw: C0
// controls other than newline, DEL, C1 controls, and characters that separate lines or reorder
// text.
func rawTerminalControls(b []byte) []rune {
	var bad []rune
	for _, r := range string(b) {
		switch {
		case r == '\n':
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f, r == 0x061c, r == 0x200e, r == 0x200f,
			r == 0x2028, r == 0x2029, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			bad = append(bad, r)
		}
	}
	return bad
}

// A label that would act on a terminal (an OSC title sequence, BEL, a C1 CSI, a bidi override)
// leaves status --json and the run log only as JSON escapes, and decodes to the same label.
func TestJSONOutputsEscapeTerminalControls(t *testing.T) {
	const label = "x\x1b]0;pwn\a\u009b2J\u202ey"
	dir := budgetDir(t, 512)
	if r := cli(t, baseEnv(dir), "run", "--label", label, "--", "true"); r.code != 0 {
		t.Fatalf("run: exit %d (stderr %q)", r.code, r.stderr)
	}
	logBytes, err := os.ReadFile(filepath.Join(dir, "log", "runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("runs.jsonl: % x", logBytes)
	if bad := rawTerminalControls(logBytes); len(bad) > 0 {
		t.Errorf("runs.jsonl carries raw %U:\n%q", bad, logBytes)
	}
	if got := runLog(t, dir)[0]["label"]; got != label {
		t.Errorf("runs.jsonl label decodes to %q, want %q", got, label)
	}

	c := openLib(t, dir)
	hold(t, c, label, 1)
	r := cli(t, baseEnv(dir), "status", "--json")
	if r.code != 0 {
		t.Fatalf("status --json: exit %d (stderr %q)", r.code, r.stderr)
	}
	i := strings.Index(r.stdout, `"label"`)
	t.Logf("status --json, from the label: % x", r.stdout[i:i+64])
	if bad := rawTerminalControls([]byte(r.stdout)); len(bad) > 0 {
		t.Errorf("status --json carries raw %U:\n%q", bad, r.stdout)
	}
	holds := statusDoc(t, dir, 0)["holds"].([]any)
	if len(holds) != 1 || holds[0].(map[string]any)["label"] != label {
		t.Errorf("status --json holds = %v, want one with the label %q", holds, label)
	}
}

func TestStatusExits3WhenDegraded(t *testing.T) {
	dir := budgetDir(t, 512)
	if r := cli(t, baseEnv(dir), "status"); r.code != 0 {
		t.Fatalf("status of a healthy directory exited %d (stderr %q)", r.code, r.stderr)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	recordDegraded(t, st, "the budget broke\nbadly")
	r := cli(t, baseEnv(dir), "status")
	if r.code != 3 {
		t.Errorf("status exited %d, want 3 (stderr %q)", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, `DEGRADED: the budget broke\nbadly (1 fail-open run since`) {
		t.Errorf("status does not report the degraded record:\n%s", r.stdout)
	}
	doc := statusDoc(t, dir, 3)
	d, ok := doc["degraded"].(map[string]any)
	if got := keysOf(d); got != "count first_ns malformed reason" {
		t.Errorf("degraded keys %s", got)
	}
	if !ok || d["reason"] != "the budget broke\nbadly" || d["count"] != float64(1) || d["malformed"] != false {
		t.Errorf("degraded = %v", doc["degraded"])
	}
	// A record that cannot be read is still degraded mode (PROTOCOL.md section 7.9).
	if err := os.WriteFile(filepath.Join(dir, "degraded"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if doc := statusDoc(t, dir, 3); doc["degraded"].(map[string]any)["malformed"] != true {
		t.Errorf("degraded = %v, want a malformed record", doc["degraded"])
	}
}

// The admission section's holder is named, with how long it has held it. The test process is
// the holder here: another process's flock refuses the CLI's probe, as it would any client's.
func TestStatusNamesSectionHolder(t *testing.T) {
	dir := stateDir(t, "")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r := cli(t, baseEnv(dir), "status"); !strings.Contains(r.stdout, "admission section: free") {
		t.Fatalf("a free section:\n%s", r.stdout)
	}
	sec, err := st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if _, _, err := sec.Format(); err != nil { // writes admit.lock's body: this pid
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	r := cli(t, baseEnv(dir), "status")
	want := fmt.Sprintf("admission section: held by pid %d for ", os.Getpid())
	if r.code != 0 || !strings.Contains(r.stdout, want) {
		t.Errorf("status exited %d, want %q in:\n%s", r.code, want, r.stdout)
	}
	doc := statusDoc(t, dir, 0)
	s := doc["section"].(map[string]any)
	h, _ := s["holder"].(map[string]any)
	if s["held"] != true || h == nil || keysOf(h) != "alive entered_ns held_for_s pid" ||
		h["pid"] != float64(os.Getpid()) || h["alive"] != true || h["held_for_s"].(float64) < 1 || h["entered_ns"].(float64) == 0 {
		t.Errorf("section = %v", s)
	}
}

// A holder admit.lock names that is gone is a stale record, not a holder: status says so.
func TestStatusStaleSectionHolder(t *testing.T) {
	dir := stateDir(t, "")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	dead := gone.Process.Pid
	body := fmt.Sprintf(`{"pid":%d,"entered_ns":%d}`, dead, time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(dir, "admit.lock"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r := cli(t, baseEnv(dir), "status")
	want := fmt.Sprintf("admission section: held; holder unknown (last recorded pid %d has exited)", dead)
	if !strings.Contains(r.stdout, want) {
		t.Errorf("status lacks %q:\n%s", want, r.stdout)
	}
	h := statusDoc(t, dir, 0)["section"].(map[string]any)["holder"].(map[string]any)
	if h["pid"] != float64(dead) || h["alive"] != false {
		t.Errorf("holder = %v, want the dead pid and alive false", h)
	}
	// An unreadable body leaves the holder unknown, with alive null.
	if err := os.WriteFile(filepath.Join(dir, "admit.lock"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	h = statusDoc(t, dir, 0)["section"].(map[string]any)["holder"].(map[string]any)
	if h["pid"] != float64(0) || h["alive"] != nil {
		t.Errorf("holder = %v, want pid 0 and alive null", h)
	}
}

func TestStatusShowsHoldNeedStraysAndIgnored(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	hold(t, c, "big", 8) // clamped to the budget; the hold reserves the need asked
	for _, d := range []string{"queue", "holds"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "admit.lock"), filepath.Join(dir, "queue", wire.TicketName(wire.ID{OrderKey: 1, PID: 1, Seq: 1}, ""))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "holds", "not-a-hold"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := cli(t, baseEnv(dir), "status")
	if r.code != 0 {
		t.Fatalf("status exited %d (stderr %q)", r.code, r.stderr)
	}
	for _, want := range []string{"big  pid", "mem 1 GiB  held", "ignored entries (malformed or stray): queue 1, holds 1"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("status lacks %q:\n%s", want, r.stdout)
		}
	}
	doc := statusDoc(t, dir, 0)
	ig := doc["ignored"].(map[string]any)
	if ig["queue"] != float64(1) || ig["holds"] != float64(1) || ig["cancel"] != float64(0) {
		t.Errorf("ignored = %v", ig)
	}
	if h := doc["holds"].([]any)[0].(map[string]any); h["need_mib"] != float64(1024) {
		t.Errorf("hold = %v, want need_mib 1024", h)
	}
}

func TestStatusUsage(t *testing.T) {
	dir := stateDir(t, "")
	for _, args := range [][]string{{"status", "--bogus"}, {"status", "extra"}, {"status", "--observe", "-1s"}} {
		if r := cli(t, baseEnv(dir), args...); r.code != 64 {
			t.Errorf("%v exited %d, want 64 (stderr %q)", args, r.code, r.stderr)
		}
	}
}

// A ticket is STALLED for the observer that saw its mtime stay the same for 10 s on the
// observer's own clock; a single reading cannot tell, and a refresh makes it waiting again.
func TestStatusFlagsStalledTicket(t *testing.T) {
	id := wire.ID{OrderKey: 100, PID: 7, Seq: 1}
	ticket := func(mtime int64) engine.Report {
		return engine.Report{Tickets: []engine.TicketInfo{{ID: id.String(), PID: 7, MtimeNS: mtime}}}
	}
	run := func(window time.Duration, mtimeAt func(call int) int64) (*stallWatcher, engine.Report) {
		clk := &fakeClock{limit: time.Hour, cancel: func() {}}
		calls := 0
		rep, w, err := observeQueue(func() (engine.Report, error) {
			calls++
			return ticket(mtimeAt(calls)), nil
		}, window, clk)
		if err != nil {
			t.Fatal(err)
		}
		return w, rep
	}
	v := func(w *stallWatcher, rep engine.Report) string {
		return buildStatus(rep, 0, w, 0).tickets[0].state
	}

	w, rep := run(0, func(int) int64 { return 5 })
	if got := v(w, rep); got != "waiting" {
		t.Errorf("a single reading: %s, want waiting", got)
	}
	w, rep = run(state.StallAfter, func(int) int64 { return 5 })
	if got := v(w, rep); got != "stalled" {
		t.Errorf("an mtime unchanged for %v: %s, want stalled", state.StallAfter, got)
	}
	w, rep = run(state.StallAfter-time.Second, func(int) int64 { return 5 })
	if got := v(w, rep); got != "waiting" {
		t.Errorf("an mtime unchanged for 9 s: %s, want waiting", got)
	}
	w, rep = run(state.StallAfter, func(int) int64 { return 5 })
	if h := buildStatus(rep, 0, w, state.StallAfter).human(); !strings.Contains(h, "  STALLED  id ") || strings.Contains(h, "not observed") {
		t.Errorf("human output of a stalled ticket after an observation:\n%s", h)
	}
	w, rep = run(0, func(int) int64 { return 5 })
	if h := buildStatus(rep, 0, w, 0).human(); strings.Contains(h, "  STALLED  id ") || !strings.Contains(h, "STALLED: not observed (use --observe 10s or more)") {
		t.Errorf("human output without an observation:\n%s", h)
	}
	w, rep = run(30*time.Second, func(call int) int64 { return int64(call) }) // refreshed at every reading
	if got := v(w, rep); got != "waiting" {
		t.Errorf("a refreshed ticket: %s, want waiting", got)
	}
}

// ---------------------------------------------------------------- doctor

func TestDoctorClear(t *testing.T) {
	dir := budgetDir(t, 512)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	recordDegraded(t, st, "fail-open: test")
	r := cli(t, baseEnv(dir), "doctor")
	if r.code != 3 || !strings.Contains(r.stdout, "degraded: fail-open: test (1 fail-open run since") {
		t.Fatalf("doctor of a degraded directory: exit %d, want 3 and the record:\n%s", r.code, r.stdout)
	}
	r = cli(t, baseEnv(dir), "doctor", "--clear")
	if r.code != 0 || !strings.Contains(r.stdout, "clear: removed the degraded record") || !strings.Contains(r.stdout, "degraded: no") {
		t.Errorf("doctor --clear: exit %d (stderr %q):\n%s", r.code, r.stderr, r.stdout)
	}
	if _, err := os.Lstat(filepath.Join(dir, "degraded")); !os.IsNotExist(err) {
		t.Errorf("the degraded record is still there: %v", err)
	}
	if r := cli(t, baseEnv(dir), "status"); r.code != 0 {
		t.Errorf("status after --clear exited %d, want 0", r.code)
	}
	if r := cli(t, baseEnv(dir), "doctor", "--clear"); r.code != 0 || !strings.Contains(r.stdout, "clear: no degraded record") {
		t.Errorf("doctor --clear without a record: exit %d:\n%s", r.code, r.stdout)
	}
}

func TestDoctorReports(t *testing.T) {
	dir := budgetDir(t, 512)
	r := cli(t, baseEnv(dir), "doctor")
	if r.code != 0 {
		t.Fatalf("doctor exited %d (stderr %q):\n%s", r.code, r.stderr, r.stdout)
	}
	for _, want := range []string{
		"filesystem: host-local",
		"permissions: every entry of the layout is the user's",
		"mem: size 512 MiB, reserved 0 MiB, free 512 MiB",
		"degraded: no",
		"no problems found",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("doctor lacks %q:\n%s", want, r.stdout)
		}
	}

	// An invalid configuration is a problem, reported with its reason, which already names the
	// file: the word "configuration" is printed once. Mutation: put the "configuration: " label
	// back in front of a reason that begins with it.
	bad := stateDir(t, "[memory]\nreserve = \"lots\"\n")
	r = cli(t, baseEnv(bad), "doctor")
	want := "\nPROBLEM: configuration " + filepath.Join(bad, "config.toml") +
		`: [memory] reserve: "lots" is not a size: want a number of GiB, such as 3 or 2.5G, or of MiB, such as 768M` + "\n"
	if r.code != 1 || !strings.Contains(r.stdout, want) {
		t.Errorf("doctor with an invalid configuration: exit %d, want the line %q in:\n%s", r.code, want, r.stdout)
	}
}

func TestDoctorRefusesLiveReset(t *testing.T) {
	cases := []struct {
		name string
		live func(t *testing.T, dir string, c *engine.Engine)
		want string
	}{
		{"hold", func(t *testing.T, dir string, c *engine.Engine) {
			hold(t, c, "gate", 2)
		}, "1 live hold (pid"},
		{"ticket", func(t *testing.T, dir string, c *engine.Engine) {
			hold(t, c, "gate", 4)
			queue(t, c, "waiting", 1)
		}, "1 live ticket (pid"},
		{"another client's hold", func(t *testing.T, dir string, c *engine.Engine) { holdOther(t, dir, 1) }, "1 live hold (pid 4"},
		{"section", func(t *testing.T, dir string, c *engine.Engine) {
			st, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			sec, err := st.Enter(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sec.Leave)
		}, "another client is inside the admission section"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := budgetDir(t, 512)
			c := openLib(t, dir)
			hold(t, c, "", 1).Close() // FORMAT, gen and a dead hold exist
			tc.live(t, dir, c)
			// A stale record of a pid with one digit: whatever pid the refusing process has, a
			// reset that marked itself as holder would change the file's length as well as its
			// bytes, so the comparison below cannot pass by the luck of equal pid lengths.
			if err := os.WriteFile(filepath.Join(dir, "admit.lock"), []byte(`{"pid":7,"entered_ns":1}`), 0o600); err != nil {
				t.Fatal(err)
			}
			before := treeContent(t, dir)
			r := cli(t, baseEnv(dir), "doctor", "--reset")
			if r.code != 75 || !strings.Contains(r.stderr, "doctor --reset refused") || !strings.Contains(r.stderr, tc.want) {
				t.Errorf("doctor --reset: exit %d, want 75 naming %q (stderr %q)", r.code, tc.want, r.stderr)
			}
			if after := treeContent(t, dir); after != before {
				t.Errorf("a refused reset changed the directory:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestDoctorResetEmptiesAnIdleDirectory(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	g := hold(t, c, "gate", 2)
	g.Close() // nothing is alive any more; FORMAT, gen and a hold-less queue remain
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	recordDegraded(t, st, "test")
	if r := cli(t, baseEnv(dir), "run", "--mem", "128M", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	lockInode := inodeOf(t, filepath.Join(dir, "admit.lock"))

	r := cli(t, baseEnv(dir), "doctor", "--reset")
	if r.code != 0 || !strings.Contains(r.stdout, "reset: removed ") {
		t.Fatalf("doctor --reset: exit %d (stderr %q):\n%s", r.code, r.stderr, r.stdout)
	}
	for _, f := range []string{"gen", "degraded"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", f, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "FORMAT")); string(b) != "coreslot-protocol 2\n" {
		t.Errorf("FORMAT after the reset %q, want coreslot-protocol 2", b)
	}
	for _, d := range []string{"queue", "holds", "cancel", "log"} {
		if got := entries(t, dir, d); len(got) != 0 {
			t.Errorf("%s/ holds %v", d, got)
		}
	}
	for _, f := range []string{"admit.lock", "config.toml"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if inodeOf(t, filepath.Join(dir, "admit.lock")) != lockInode {
		t.Error("admit.lock was replaced")
	}
	if r := cli(t, baseEnv(dir), "run", "--mem", "128M", "--", "true"); r.code != 0 {
		t.Errorf("a run after the reset exited %d (stderr %q)", r.code, r.stderr)
	}
}

// A reset removes what it can (best effort) and, when an entry cannot be removed, says how many
// entries it removed and how many it could not, besides the error.
func TestDoctorResetReportsPartialRemoval(t *testing.T) {
	dir := stateDir(t, "")
	if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	// A directory under a ticket's name is a stray no client owns, and os.Remove cannot remove it
	// while it holds a file.
	stuck := filepath.Join(dir, "queue", "00000000000000000001-0000000001-000001")
	if err := os.MkdirAll(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := cli(t, baseEnv(dir), "doctor", "--reset")
	if r.code != 1 {
		t.Fatalf("doctor --reset left %s behind and exited %d, want 1: a state problem is never 70", stuck, r.code)
	}
	// The run log and gen were removed although the stuck entry could not be.
	if !strings.Contains(r.stderr, "removed 2 entries; 1 could not be removed: ") {
		t.Errorf("stderr %q: want how many entries were removed (2) and how many could not be (1)", r.stderr)
	}
	for _, f := range []string{"gen", filepath.Join("log", "runs.jsonl")} {
		if _, err := os.Lstat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", f, err)
		}
	}
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return uint64(st.Ino)
}

// ---------------------------------------------------------------- cancel

func TestCancelByLabelRefusesAmbiguous(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	hold(t, c, "gate", 4)
	a := queue(t, c, "twin", 1)
	b := queue(t, c, "twin", 1)
	solo := queue(t, c, "solo", 1)

	r := cli(t, baseEnv(dir), "cancel", "--label", "twin")
	if r.code != 75 || !strings.Contains(r.stderr, "2 waiting tickets have the label twin") {
		t.Errorf("cancel --label twin: exit %d, want 75 naming the two (stderr %q)", r.code, r.stderr)
	}
	if got := entries(t, dir, "cancel"); len(got) != 0 {
		t.Errorf("an ambiguous label published cancel requests: %v", got)
	}
	for _, w := range []*engine.Ticket{a, b, solo} {
		if r, err := w.Poll(); err != nil || admitted(r) {
			t.Errorf("a waiter was disturbed: admitted %v, %v", admitted(r), err)
		}
	}

	if r := cli(t, baseEnv(dir), "cancel", "--label", "solo"); r.code != 0 {
		t.Fatalf("cancel --label solo: exit %d (stderr %q)", r.code, r.stderr)
	}
	if _, err := solo.Poll(); err != engine.ErrCanceled {
		t.Errorf("the cancelled waiter's Poll = %v, want ErrCanceled", err)
	}
	if r, err := a.Poll(); err != nil || admitted(r) {
		t.Errorf("a twin was disturbed: admitted %v, %v", admitted(r), err)
	}
}

func TestCancelByIDAndRefusals(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	hold(t, c, "gate", 4)
	w := queue(t, c, "victim", 1)

	var id string
	for _, tk := range statusDoc(t, dir, 0)["tickets"].([]any) {
		id = tk.(map[string]any)["id"].(string)
	}
	if r := cli(t, baseEnv(dir), "cancel", id); r.code != 0 || !strings.Contains(r.stdout, id) {
		t.Fatalf("cancel %s: exit %d (stderr %q)", id, r.code, r.stderr)
	}
	if r := cli(t, baseEnv(dir), "cancel", id); r.code != 0 {
		t.Errorf("cancelling twice: exit %d, want 0 (already requested is not an error; stderr %q)", r.code, r.stderr)
	}
	if _, err := w.Poll(); err != engine.ErrCanceled {
		t.Errorf("Poll = %v, want ErrCanceled", err)
	}

	gone := wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String()
	if r := cli(t, baseEnv(dir), "cancel", gone); r.code != 75 || !strings.Contains(r.stderr, "no waiting ticket") {
		t.Errorf("cancel of an unknown ticket: exit %d, want 75 (stderr %q)", r.code, r.stderr)
	}
	if r := cli(t, baseEnv(dir), "cancel", "--label", "nobody"); r.code != 75 {
		t.Errorf("cancel --label of no ticket: exit %d, want 75 (stderr %q)", r.code, r.stderr)
	}
	for _, args := range [][]string{{"cancel"}, {"cancel", "not-an-id"}, {"cancel", id, "--label", "x"}, {"cancel", "--label", "x", id}} {
		if r := cli(t, baseEnv(dir), args...); r.code != 64 {
			t.Errorf("%v: exit %d, want 64 (stderr %q)", args, r.code, r.stderr)
		}
	}
}

// ---------------------------------------------------------------- version

var versionLine = regexp.MustCompile(`^coreslot \S+ \(protocol formats: ([0-9, ]+)\)\n$`)

func TestVersionListsFormats(t *testing.T) {
	r := cli(t, baseEnv(stateDir(t, "")), "version")
	m := versionLine.FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil {
		t.Fatalf("version: exit %d, stdout %q", r.code, r.stdout)
	}
	if want := formatsOfThisBuild(); m[1] != want {
		t.Errorf("formats %q, want %q", m[1], want)
	}
	if r := cli(t, baseEnv(stateDir(t, "")), "--version"); r.code != 0 || !versionLine.MatchString(r.stdout) {
		t.Errorf("--version: exit %d, stdout %q", r.code, r.stdout)
	}
	if r := cli(t, baseEnv(stateDir(t, "")), "version", "extra"); r.code != 64 {
		t.Errorf("version extra: exit %d, want 64", r.code)
	}
}

// ---------------------------------------------------------------- read-only commands

// status and doctor on a machine where nothing has run create nothing: not the state directory,
// not res/, not admit.lock. A missing directory is reported as such and is not a failure.
func TestStatusAndDoctorCreateNothing(t *testing.T) {
	for _, shape := range []string{"missing", "empty"} {
		t.Run(shape, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "state dir")
			if shape == "empty" {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			before := tree(t, root)
			cmds := [][]string{{"status"}, {"status", "--json"}, {"doctor"}, {"doctor", "--clear"}}
			noTicket := wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String()
			if shape == "missing" { // on an existing directory a cancel enters the section, which creates admit.lock
				if r := cli(t, baseEnv(dir), "cancel", noTicket); r.code != 75 {
					t.Errorf("cancel on a missing directory exited %d, want 75 (stderr %q)", r.code, r.stderr)
				}
			}
			if r := cli(t, baseEnv(dir), "cancel", "--label", "x"); r.code != 75 {
				t.Errorf("cancel --label on %s directory exited %d, want 75 (stderr %q)", shape, r.code, r.stderr)
			}
			if shape == "missing" { // a reset of an existing directory enters the section, which creates admit.lock
				cmds = append(cmds, []string{"doctor", "--reset"})
			}
			for _, args := range cmds {
				r := cli(t, baseEnv(dir), args...)
				if r.code != 0 {
					t.Errorf("%v exited %d (stderr %q):\n%s", args, r.code, r.stderr, r.stdout)
				}
				if shape == "missing" && args[0] == "status" && args[len(args)-1] != "--json" &&
					!strings.Contains(r.stdout, "no state directory yet (nothing has run)") {
					t.Errorf("%v does not say nothing has run:\n%s", args, r.stdout)
				}
				if after := tree(t, root); after != before {
					t.Fatalf("%v created entries:\nbefore:\n%s\nafter:\n%s", args, before, after)
				}
			}
			if shape == "missing" {
				doc := statusDoc(t, dir, 0)
				if doc["state_dir_exists"] != false {
					t.Errorf("state_dir_exists = %v, want false", doc["state_dir_exists"])
				}
				mem := doc["mem"].(map[string]any)
				if mem["reserved_mib"] != float64(0) || mem["free_mib"] != mem["size_mib"] {
					t.Errorf("an empty budget is expected, got %v", mem)
				}
			}
		})
	}
}

// A directory of format 1, with its res/ files and a dead hold, is not used: a run runs
// unbudgeted at once with one notice that says what to do, --strict refuses, and doctor --reset
// recreates the directory as format 2, its res/ gone. Mutations: keep format 1 in
// store.Formats (the run queues on it and prints nothing); skip the res/ removal in Reset.
func TestForeignFormatFailsOpenAndResets(t *testing.T) {
	dir := stateDir(t, "")
	for _, d := range []string{"res/mem", "holds"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"FORMAT":    "coreslot-protocol 1\n",
		"res/mem/0": "",
		"holds/01791720600000000000-0000004711-000001-old": `{"label":"old","grant":{"mem":[0]},"start_ns":1791720600000000000}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const notice = "state format 1 is not supported; run coreslot doctor --reset"
	r := cli(t, baseEnv(dir), "run", "--", "true")
	if r.code != 0 || strings.Count(r.stderr, "\n") != 1 || !strings.Contains(r.stderr, notice) {
		t.Errorf("run: exit %d, stderr %q; want 0 and one notice containing %q", r.code, r.stderr, notice)
	}
	if r := cli(t, baseEnv(dir), "run", "--strict", "--", "true"); r.code != 75 || !strings.Contains(r.stderr, notice) {
		t.Errorf("run --strict: exit %d, stderr %q; want 75 and the notice", r.code, r.stderr)
	}
	if r := cli(t, baseEnv(dir), "doctor", "--reset"); r.code != 0 {
		t.Fatalf("doctor --reset: exit %d (stderr %q)", r.code, r.stderr)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "FORMAT")); err != nil || string(b) != "coreslot-protocol 2\n" {
		t.Errorf("FORMAT after the reset: %q, %v; want coreslot-protocol 2", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "res")); !os.IsNotExist(err) {
		t.Errorf("res/ survived the reset: %v", err)
	}
	if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 || r.stderr != "" {
		t.Errorf("a run after the reset: exit %d, stderr %q; want 0 and silence", r.code, r.stderr)
	}
}

// A directory whose FORMAT is well-formed but newer is not this client's to reset or clear;
// a malformed one cannot be cleared (a reset repairs it).
func TestDoctorFormatRefusals(t *testing.T) {
	setup := func(t *testing.T, format string) string {
		dir := budgetDir(t, 512)
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		recordDegraded(t, st, "test")
		for _, d := range []string{"queue"} {
			if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "queue", "v2-ticket"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "FORMAT"), []byte(format), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	t.Run("clear under format 3", func(t *testing.T) {
		dir := setup(t, "coreslot-protocol 3\n")
		before := tree(t, dir)
		r := cli(t, baseEnv(dir), "doctor", "--clear")
		if r.code != 75 || !strings.Contains(r.stderr, "state format 3 is not supported; run coreslot doctor --reset") {
			t.Errorf("doctor --clear: exit %d, want 75 (stderr %q)", r.code, r.stderr)
		}
		if after := tree(t, dir); after != before {
			t.Errorf("the refused clear changed the tree:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})
	t.Run("clear under a malformed FORMAT, then reset repairs it", func(t *testing.T) {
		dir := setup(t, "coreslot-protocol x\n")
		r := cli(t, baseEnv(dir), "doctor", "--clear")
		if r.code != 75 || !strings.Contains(r.stderr, "FORMAT is malformed") || !strings.Contains(r.stderr, "doctor --reset") {
			t.Errorf("doctor --clear: exit %d, want 75 pointing at --reset (stderr %q)", r.code, r.stderr)
		}
		if r := cli(t, baseEnv(dir), "doctor", "--reset"); r.code != 0 {
			t.Errorf("doctor --reset: exit %d (stderr %q)", r.code, r.stderr)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "FORMAT")); string(b) != "coreslot-protocol 2\n" {
			t.Errorf("FORMAT after the reset %q, want coreslot-protocol 2", b)
		}
	})
}

// Error text that carries user input (here a state directory path with a newline and an escape in
// it) is escaped like a label: each message stays on one line.
func TestCommandErrorsAreEscaped(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "x\ny\x1b[31m") // a file where the directory belongs
	if err := os.WriteFile(bad, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := baseEnv(bad)
	id := wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String()
	for _, args := range [][]string{{"status"}, {"status", "--json"}, {"cancel", id}, {"doctor", "--clear"}, {"doctor"}} {
		r := cli(t, env, args...)
		out := r.stderr + r.stdout
		if r.code == 0 || !strings.Contains(out, `x\ny\x1b[31m`) {
			t.Errorf("%v: exit %d, want a failure that quotes the directory escaped:\n%q", args, r.code, out)
		}
		if strings.ContainsAny(out, "\x1b\r") || strings.Count(r.stderr, "\n") > 1 {
			t.Errorf("%v printed a raw control character or several error lines:\n%q", args, out)
		}
	}
	// run fails open, refuses under --strict, or cannot start a command whose name is user text
	// too: every notice it prints is one line of its own, starting with "coreslot: ".
	for _, args := range [][]string{{"run", "--", "true"}, {"run", "--strict", "--", "true"},
		{"run", "--", "./no\nsuch\x1b[31m"}} {
		r := cli(t, env, args...)
		if !strings.Contains(r.stderr, `x\ny\x1b[31m`) {
			t.Errorf("%q: exit %d, want a notice that quotes the directory escaped:\n%q", args, r.code, r.stderr)
		}
		if strings.ContainsAny(r.stderr, "\x1b\r") {
			t.Errorf("%q printed a raw control character:\n%q", args, r.stderr)
		}
		for _, l := range strings.Split(strings.TrimSuffix(r.stderr, "\n"), "\n") {
			if !strings.HasPrefix(l, "coreslot: ") {
				t.Errorf("%q printed a line that is not a notice of its own: %q", args, l)
			}
		}
	}
	// A flag the run does not know is named escaped in the usage error.
	if r := cli(t, env, "run", "--no\x1b[31m", "--", "true"); r.code != exitUsage || strings.ContainsAny(r.stderr, "\x1b\r") {
		t.Errorf("run with an unknown flag: exit %d, want %d without a raw control character:\n%q", r.code, exitUsage, r.stderr)
	}
}

// ---------------------------------------------------------------- help, validation, exit statuses

// Every --help prints on stdout and exits 0; run's help gives each flag a line and its default.
func TestHelpGoesToStdout(t *testing.T) {
	dir := stateDir(t, "")
	for _, args := range [][]string{
		{"help"}, {"--help"}, {"run", "--help"}, {"run", "-h"}, {"status", "--help"}, {"cancel", "--help"},
		{"doctor", "--help"}, {"version", "--help"}, {"version", "-h"},
	} {
		r := cli(t, baseEnv(dir), args...)
		if r.code != 0 || !strings.HasPrefix(r.stdout, "usage: coreslot") || r.stderr != "" {
			t.Errorf("%v: exit %d, want 0 with the usage on stdout only (stdout %q, stderr %q)", args, r.code, r.stdout, r.stderr)
		}
	}
	help := cli(t, baseEnv(dir), "run", "--help").stdout
	for _, want := range []string{
		"  --mem SIZE ", "such as 3G or 768M",
		"  --label L ", "  --timeout D ", "(default 0, no limit)",
		"  --on-timeout run|fail ", "(default fail)", "  --no-wait ", "  --strict ", "  --independent ",
		"  --same-group ", "signal the group, not coreslot's pid", "  --refusal-code N ", "(default 75)",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("run --help lacks %q:\n%s", want, help)
		}
	}
	if q := entries(t, dir, "queue"); len(q) != 0 {
		t.Errorf("a --help queued: %q", q)
	}
}

// A negative duration is a usage error that says what is wanted; a zero --timeout is no limit.
func TestNegativeDurationsAreUsageErrors(t *testing.T) {
	dir := stateDir(t, "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"run", "--timeout", "-1s", "--", "true"}, "coreslot: --timeout -1s: want a duration of 0 or more (0 means no limit)"},
		{[]string{"status", "--observe", "-1s"}, "coreslot: status: --observe -1s: want a duration of 0 or more"},
	} {
		r := cli(t, baseEnv(dir), c.args...)
		if r.code != 64 || !strings.HasPrefix(r.stderr, c.want+"\n") {
			t.Errorf("%v: exit %d, want 64 and %q first (stderr %q)", c.args, r.code, c.want, r.stderr)
		}
	}
	if r := cli(t, baseEnv(dir), "run", "--timeout", "0s", "--", "true"); r.code != 0 {
		t.Errorf("--timeout 0s: exit %d, want 0 (stderr %q)", r.code, r.stderr)
	}
}

// A state directory that cannot be used is a state error, never an internal error (70): status
// exits 3 as runs fail open, cancel refuses (75), and doctor reports a problem (1).
func TestUnusableStateDirectoryExitStatuses(t *testing.T) {
	groupWritable := stateDir(t, "")
	if err := os.Chmod(groupWritable, 0o770); err != nil {
		t.Fatal(err)
	}
	id := wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String()
	for _, dir := range []string{groupWritable, "relative/state"} {
		for _, c := range []struct {
			args []string
			code int
			want string
		}{
			{[]string{"status"}, 3, "coreslot: status: state error: "},
			{[]string{"status", "--json"}, 3, "coreslot: status: state error: "},
			{[]string{"cancel", id}, 75, "coreslot: cancel refused: state error: "},
			{[]string{"cancel", "--label", "x"}, 75, "coreslot: cancel refused: state error: "},
			{[]string{"doctor", "--clear"}, 1, "coreslot: doctor --clear: state error: "},
			{[]string{"doctor", "--reset"}, 1, "coreslot: doctor --reset: state error: "},
			{[]string{"doctor"}, 1, ""},
		} {
			r := cli(t, baseEnv(dir), c.args...)
			if r.code != c.code || !strings.HasPrefix(r.stderr, c.want) {
				t.Errorf("%s: %v: exit %d, want %d and stderr starting %q (stderr %q)", dir, c.args, r.code, c.code, c.want, r.stderr)
			}
			if c.args[0] == "status" && r.stdout != "" {
				t.Errorf("%s: %v printed a report it could not read:\n%s", dir, c.args, r.stdout)
			}
		}
	}
}

// An entry of the state directory that cannot be read is a state error like any other (PROTOCOL.md
// section 19): never exit 70. status exits 3, cancel refuses with 75 and doctor --clear and
// --reset exit 1, with the I/O error's own text. Each case reads the entry before it can act.
func TestUnreadableStateEntryExitStatuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 entry")
	}
	id := wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String()
	for _, c := range []struct {
		entry string
		args  []string
		code  int
		want  string
	}{
		{"queue", []string{"status"}, 3, "coreslot: status: open "},
		{"queue", []string{"cancel", id}, 75, "coreslot: cancel refused: open "},
		{"queue", []string{"cancel", "--label", "x"}, 75, "coreslot: cancel refused: open "},
		{"queue", []string{"doctor", "--reset"}, 1, "coreslot: doctor --reset: open "},
		{"holds", []string{"status"}, 3, "coreslot: status: open "},
		{"holds", []string{"cancel", "--label", "x"}, 75, "coreslot: cancel refused: open "},
		{"holds", []string{"doctor", "--reset"}, 1, "coreslot: doctor --reset: open "},
		{"gen", []string{"status"}, 3, "coreslot: status: open "},
		{"gen", []string{"cancel", id}, 75, "coreslot: cancel refused: open "},
		{"gen", []string{"doctor", "--clear"}, 1, "coreslot: doctor --clear: open "},
		{"degraded", []string{"status"}, 3, "coreslot: status: open "},
		{"degraded", []string{"cancel", "--label", "x"}, 75, "coreslot: cancel refused: open "},
		{"degraded", []string{"doctor", "--clear"}, 1, "coreslot: doctor --clear: open "},
		{"FORMAT", []string{"doctor", "--clear"}, 1, "coreslot: doctor --clear: open "},
	} {
		dir := stateDir(t, "")
		if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
			t.Fatalf("setup run: exit %d (stderr %q)", r.code, r.stderr)
		}
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		recordDegraded(t, st, "fail-open: test")
		p := filepath.Join(dir, c.entry)
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		r := cli(t, baseEnv(dir), c.args...)
		if err := os.Chmod(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if r.code != c.code || !strings.HasPrefix(r.stderr, c.want) || !strings.Contains(r.stderr, "permission denied") {
			t.Errorf("%s unreadable: %v: exit %d, want %d and stderr starting %q (stderr %q)", c.entry, c.args, r.code, c.code, c.want, r.stderr)
		}
	}
}

// An invalid configuration leaves the budget unknown: status says so rather than "none declared"
// and exits 3, since runs fail open. The file's error, which names it, follows the label with the
// word "configuration" printed once. Mutation: print the reason whole after the label.
func TestStatusWithInvalidConfiguration(t *testing.T) {
	dir := stateDir(t, "[memory]\nreserve = \"lots\"\n")
	r := cli(t, baseEnv(dir), "status")
	want := "\nCONFIGURATION ERROR: " + filepath.Join(dir, "config.toml") +
		`: [memory] reserve: "lots" is not a size: want a number of GiB, such as 3 or 2.5G, or of MiB, such as 768M` + "\n"
	if r.code != 3 || !strings.Contains(r.stdout, "\nmem: not known, the configuration cannot be read (runs fail open until it is fixed)\n") ||
		strings.Contains(r.stdout, "none declared") || !strings.Contains(r.stdout, want) {
		t.Errorf("status: exit %d, want 3, the line %q and the budget reported unknown (stderr %q):\n%s", r.code, want, r.stderr, r.stdout)
	}
	doc := statusDoc(t, dir, 3)
	if doc["config_error"] == "" || doc["mem"] != nil {
		t.Errorf("status --json: config_error %q, mem %v; want an error and a null mem", doc["config_error"], doc["mem"])
	}
}

// A malformed FORMAT is a state error too: runs fail open, so status exits 3.
func TestStatusWithMalformedFormat(t *testing.T) {
	dir := stateDir(t, "")
	if err := os.WriteFile(filepath.Join(dir, "FORMAT"), []byte("coreslot-protocol x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := cli(t, baseEnv(dir), "status"); r.code != 3 || !strings.Contains(r.stdout, "format: MALFORMED (") {
		t.Errorf("status: exit %d, want 3 reporting FORMAT malformed (stderr %q):\n%s", r.code, r.stderr, r.stdout)
	}
}

// FORMAT naming format 0 is a format this client does not implement (PROTOCOL.md section 7.6),
// not an absent FORMAT: status and doctor say so, and doctor counts it a problem, since every run
// there fails open. Mutation: take Format == 0 for absence again, before FormatUnderstood, in
// either command and its case turns red; a line instead of a problem exits doctor 0.
func TestStatusAndDoctorReportFormatZero(t *testing.T) {
	dir := stateDir(t, "")
	if err := os.WriteFile(filepath.Join(dir, "FORMAT"), []byte("coreslot-protocol 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := cli(t, baseEnv(dir), "status"); r.code != 0 ||
		!strings.Contains(r.stdout, "\nformat: 0, which this client does not implement\n") {
		t.Errorf("status: exit %d, want 0 naming format 0 unimplemented (stderr %q):\n%s", r.code, r.stderr, r.stdout)
	}
	if r := cli(t, baseEnv(dir), "doctor"); r.code != 1 ||
		!strings.Contains(r.stdout, "format: 0, which this client does not implement; runs fail open until `coreslot doctor --reset`\n") {
		t.Errorf("doctor: exit %d, want 1 naming format 0 unimplemented (stderr %q):\n%s", r.code, r.stderr, r.stdout)
	}
	doc := statusDoc(t, dir, 0)
	if doc["format"] != float64(0) || doc["format_present"] != true || doc["format_understood"] != false {
		t.Errorf("status --json: format %v, format_present %v, format_understood %v; want 0, true, false",
			doc["format"], doc["format_present"], doc["format_understood"])
	}
	if err := os.Remove(filepath.Join(dir, "FORMAT")); err != nil {
		t.Fatal(err)
	}
	if r := cli(t, baseEnv(dir), "status"); !strings.Contains(r.stdout, "\nformat: not written yet (the first client writes 2)\n") {
		t.Errorf("status without FORMAT: exit %d:\n%s", r.code, r.stdout)
	}
	if doc := statusDoc(t, dir, 0); doc["format_present"] != false || doc["format_understood"] != true {
		t.Errorf("status --json without FORMAT: format_present %v, format_understood %v; want false, true",
			doc["format_present"], doc["format_understood"])
	}
}

// doctor names the section's holder the way status does.
func TestDoctorNamesSectionHolder(t *testing.T) {
	dir := stateDir(t, "")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if _, _, err := sec.Format(); err != nil { // writes admit.lock's body: this pid
		t.Fatal(err)
	}
	r := cli(t, baseEnv(dir), "doctor")
	want := fmt.Sprintf("\nadmission section: held by pid %d for ", os.Getpid())
	if !strings.Contains(r.stdout, want) {
		t.Errorf("doctor: exit %d, want %q in:\n%s", r.code, want, r.stdout)
	}
}

// The engine's refusal reaches the user once prefixed: `coreslot: cancel refused: ...`.
func TestCancelRefusalHasOnePrefix(t *testing.T) {
	dir := stateDir(t, "")
	if err := os.WriteFile(filepath.Join(dir, "FORMAT"), []byte("coreslot-protocol 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := cli(t, baseEnv(dir), "cancel", wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String())
	want := "coreslot: cancel refused: state format 3 is not supported; run coreslot doctor --reset\n"
	if r.code != 75 || r.stderr != want {
		t.Errorf("cancel under format 3: exit %d, stderr %q; want 75 and %q", r.code, r.stderr, want)
	}
}

// A client stopped inside the admission section does not keep cancel waiting: it refuses within
// a bound, naming the holder admit.lock records.
func TestCancelRefusesWhileSectionHeld(t *testing.T) {
	dir := stateDir(t, "")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := st.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if _, _, err := sec.Format(); err != nil { // writes admit.lock's body: this pid
		t.Fatal(err)
	}
	start := time.Now()
	r := cli(t, baseEnv(dir), "cancel", wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String())
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("cancel refused after %v, want within 5 s (load: %s)", took, uptime())
	}
	pid := fmt.Sprintf("pid %d", os.Getpid())
	if r.code != 75 || !strings.HasPrefix(r.stderr, "coreslot: cancel refused: ") || !strings.Contains(r.stderr, pid) ||
		strings.Count(r.stderr, "coreslot:") != 1 || strings.Count(r.stderr, "\n") != 1 {
		t.Errorf("cancel: exit %d, want 75 and one line refusing, naming %s (stderr %q)", r.code, pid, r.stderr)
	}
	if got := entries(t, dir, "cancel"); len(got) != 0 {
		t.Errorf("a refused cancel published %v", got)
	}
}

// A refused reset names each live hold and ticket by its label, as status shows it.
func TestDoctorResetRefusalNamesLabels(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	hold(t, c, "hold:three", 4)
	queue(t, c, "ticket:one", 1)
	r := cli(t, baseEnv(dir), "doctor", "--reset")
	if r.code != 75 {
		t.Fatalf("doctor --reset: exit %d, want 75 (stderr %q)", r.code, r.stderr)
	}
	for _, want := range []string{"hold:three", "ticket:one"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("doctor --reset refusal lacks the label %q: %q", want, r.stderr)
		}
	}
}
