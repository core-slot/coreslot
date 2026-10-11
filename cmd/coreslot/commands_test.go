//go:build unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot"
	"github.com/Nezhinskiy/coreslot/internal/state"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// ---------------------------------------------------------------- fixtures

// openLib opens the library over dir, as the test's own client: what it adopts and enqueues is
// held by the test process, so the CLI the test runs sees live holds and tickets.
func openLib(t *testing.T, dir string) *coreslot.Client {
	t.Helper()
	c, err := coreslot.Open(coreslot.Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func rng(min, max int) coreslot.Range { return coreslot.Range{Min: min, Max: max} }

// hold adopts a grant for the label and keeps it until the test ends.
func hold(t *testing.T, c *coreslot.Client, label string, need map[string]coreslot.Range) *coreslot.Grant {
	t.Helper()
	g, _, err := c.Adopt(coreslot.Request{Label: label, Need: need})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

// queue enqueues a request and withdraws it when the test ends.
func queue(t *testing.T, c *coreslot.Client, label string, need map[string]coreslot.Range) *coreslot.Waiter {
	t.Helper()
	w, err := c.Enqueue(coreslot.Request{Label: label, Need: need})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Cancel() })
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

// ---------------------------------------------------------------- status

func TestStatusJSONSchema(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	c := openLib(t, dir)
	hold(t, c, "gate", map[string]coreslot.Range{"cpu": rng(3, 3)}) // tokens 0..2
	queue(t, c, "waiting", map[string]coreslot.Range{"cpu": rng(2, 4)})
	holdToken(t, dir, "cpu", 3) // locked, and no hold lists it

	doc := statusDoc(t, dir, 0)
	const top = "cancel_requests config_error dead_holds degraded format format_error format_understood gen holds ignored observed_s reconciled_ns resources schema section state_dir tickets"
	if got := keysOf(doc); got != top {
		t.Errorf("top-level keys\n got %s\nwant %s", got, top)
	}
	if doc["schema"] != float64(1) {
		t.Errorf("schema = %v, want 1", doc["schema"])
	}
	if doc["degraded"] != nil {
		t.Errorf("degraded = %v, want null", doc["degraded"])
	}
	if got := keysOf(doc["section"].(map[string]any)); got != "entered_ns held held_for_s pid" {
		t.Errorf("section keys %s", got)
	}

	cpu := doc["resources"].(map[string]any)["cpu"].(map[string]any)
	if got := keysOf(cpu); got != "free locked size unlisted_locked" {
		t.Errorf("resource keys %s", got)
	}
	if cpu["size"] != float64(4) || cpu["locked"] != float64(4) || cpu["free"] != float64(0) || cpu["unlisted_locked"] != float64(1) {
		t.Errorf("cpu = %v, want size 4, locked 4, free 0 and one locked token no hold lists", cpu)
	}

	holds := doc["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v", holds)
	}
	h := holds[0].(map[string]any)
	const holdKeys = "adopted clamped grant held_for_s label name pid start_ns unbudgeted unbudgeted_resources"
	if got := keysOf(h); got != holdKeys {
		t.Errorf("hold keys\n got %s\nwant %s", got, holdKeys)
	}
	if h["label"] != "gate" || h["adopted"] != true || h["grant"].(map[string]any)["cpu"] != float64(3) || h["pid"] != float64(os.Getpid()) {
		t.Errorf("hold = %v", h)
	}

	tickets := doc["tickets"].([]any)
	if len(tickets) != 1 {
		t.Fatalf("tickets = %v", tickets)
	}
	tk := tickets[0].(map[string]any)
	const ticketKeys = "created_ns id label need pid position priority stalled state waited_s"
	if got := keysOf(tk); got != ticketKeys {
		t.Errorf("ticket keys\n got %s\nwant %s", got, ticketKeys)
	}
	if tk["label"] != "waiting" || tk["position"] != float64(1) || tk["state"] != "waiting" || tk["stalled"] != false {
		t.Errorf("ticket = %v", tk)
	}
	if need := tk["need"].(map[string]any)["cpu"].(map[string]any); need["min"] != float64(2) || need["max"] != float64(4) {
		t.Errorf("need = %v", need)
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

func TestStatusEscapesLabels(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	c := openLib(t, dir)
	labels := []string{
		"line one\nline two",
		"\x1b[31mred\x1b[0m",
		"tab\there\rcarriage",
		"rtl\u202eoverride",
		`back\slash "quote" <tag>`,
	}
	hold(t, c, labels[0], map[string]coreslot.Range{"cpu": rng(4, 4)})
	for _, l := range labels[1:] {
		queue(t, c, l, map[string]coreslot.Range{"cpu": rng(1, 1)})
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

func TestStatusExits3WhenDegraded(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	if r := cli(t, baseEnv(dir), "status"); r.code != 0 {
		t.Fatalf("status of a healthy directory exited %d (stderr %q)", r.code, r.stderr)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDegraded("the budget broke\nbadly"); err != nil {
		t.Fatal(err)
	}
	r := cli(t, baseEnv(dir), "status")
	if r.code != 3 {
		t.Errorf("status exited %d, want 3 (stderr %q)", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, `DEGRADED: the budget broke\nbadly (1 fail-open run since`) {
		t.Errorf("status does not report the degraded record:\n%s", r.stdout)
	}
	doc := statusDoc(t, dir, 3)
	d, ok := doc["degraded"].(map[string]any)
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
	if _, _, err := sec.Format([]int{1}); err != nil { // writes admit.lock's body: this pid
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
	if s["held"] != true || s["pid"] != float64(os.Getpid()) || s["held_for_s"].(float64) < 1 {
		t.Errorf("section = %v", s)
	}
}

func TestStatusShowsHoldFlagsStraysAndIgnored(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	c := openLib(t, dir)
	hold(t, c, "big", map[string]coreslot.Range{"cpu": rng(8, 8), "gpu": rng(1, 1)}) // clamped; gpu undeclared
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
	for _, want := range []string{"big  pid", "clamped cpu (asked 8)", "unbudgeted gpu", "adopted", "ignored entries (malformed or stray): queue 1, holds 1"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("status lacks %q:\n%s", want, r.stdout)
		}
	}
	doc := statusDoc(t, dir, 0)
	ig := doc["ignored"].(map[string]any)
	if ig["queue"] != float64(1) || ig["holds"] != float64(1) || ig["cancel"] != float64(0) {
		t.Errorf("ignored = %v", ig)
	}
	h := doc["holds"].([]any)[0].(map[string]any)
	if h["clamped"].(map[string]any)["cpu"] != float64(8) || !slices.Equal(anyStrings(h["unbudgeted_resources"]), []string{"gpu"}) {
		t.Errorf("hold = %v", h)
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
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
	ticket := func(mtime int64) coreslot.Report {
		return coreslot.Report{Tickets: []coreslot.TicketInfo{{ID: id.String(), PID: 7, MtimeNS: mtime}}}
	}
	run := func(window time.Duration, mtimeAt func(call int) int64) (*stallWatcher, coreslot.Report) {
		var now time.Duration
		clk := obsClock{now: func() time.Duration { return now }, sleep: func(d time.Duration) { now += d }}
		calls := 0
		rep, w, err := observeQueue(func() (coreslot.Report, error) {
			calls++
			return ticket(mtimeAt(calls)), nil
		}, window, clk)
		if err != nil {
			t.Fatal(err)
		}
		return w, rep
	}
	v := func(w *stallWatcher, rep coreslot.Report) string {
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
	w, rep = run(30*time.Second, func(call int) int64 { return int64(call) }) // refreshed at every reading
	if got := v(w, rep); got != "waiting" {
		t.Errorf("a refreshed ticket: %s, want waiting", got)
	}
}

// ---------------------------------------------------------------- doctor

func TestDoctorClear(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDegraded("fail-open: test"); err != nil {
		t.Fatal(err)
	}
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

func TestDoctorReportsLimits(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\npg-local = 2\n")
	r := cli(t, baseEnv(dir), "doctor")
	if r.code != 0 {
		t.Fatalf("doctor exited %d (stderr %q):\n%s", r.code, r.stderr, r.stdout)
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"filesystem: host-local",
		"permissions: every entry of the layout is the user's",
		"resource cpu: size 4, locked 0, free 4",
		"resource pg-local: size 2, locked 0, free 2",
		fmt.Sprintf("descriptor limit: soft %s, hard %s;", limitName(uint64(lim.Cur)), limitName(uint64(lim.Max))),
		"degraded: no",
		"no problems found",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("doctor lacks %q:\n%s", want, r.stdout)
		}
	}

	// A hard limit below what a large grant needs is a problem.
	cmd := shCmd(t, baseEnv(dir), `ulimit -n 300 && exec "$0" doctor`, cliBinary(t))
	out, err := cmd.CombinedOutput()
	if code := exitStatus(cmd.ProcessState); code != 1 || !strings.Contains(string(out), "PROBLEM: the hard descriptor limit 300 is below 600") {
		t.Errorf("doctor under ulimit -n 300: exit %d, %v:\n%s", code, err, out)
	}

	// An invalid configuration is a problem, reported with its reason.
	bad := stateDir(t, "[resources]\ncpu = \"lots\"\n")
	r = cli(t, baseEnv(bad), "doctor")
	if r.code != 1 || !strings.Contains(r.stdout, "PROBLEM: configuration:") {
		t.Errorf("doctor with an invalid configuration: exit %d:\n%s", r.code, r.stdout)
	}
}

// limitName is how the report spells a limit: a number, or unlimited.
func limitName(n uint64) string {
	if n >= 1<<62 {
		return "unlimited"
	}
	return fmt.Sprint(n)
}

func TestDoctorRefusesLiveReset(t *testing.T) {
	cases := []struct {
		name string
		live func(t *testing.T, dir string, c *coreslot.Client)
		want string
	}{
		{"hold", func(t *testing.T, dir string, c *coreslot.Client) {
			hold(t, c, "gate", map[string]coreslot.Range{"cpu": rng(2, 2)})
		}, "1 live hold (pid"},
		{"ticket", func(t *testing.T, dir string, c *coreslot.Client) {
			hold(t, c, "gate", map[string]coreslot.Range{"cpu": rng(4, 4)})
			queue(t, c, "waiting", map[string]coreslot.Range{"cpu": rng(1, 1)})
		}, "1 live ticket (pid"},
		{"token", func(t *testing.T, dir string, c *coreslot.Client) { holdToken(t, dir, "cpu", 1) }, "locked tokens (cpu 1)"},
		{"section", func(t *testing.T, dir string, c *coreslot.Client) {
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
			dir := stateDir(t, "[resources]\ncpu = 4\n")
			c := openLib(t, dir)
			g, _, err := c.Adopt(coreslot.Request{Need: map[string]coreslot.Range{"cpu": rng(1, 1)}})
			if err != nil { // FORMAT, gen and the token files exist
				t.Fatal(err)
			}
			g.Close()
			tc.live(t, dir, c)
			before := tree(t, dir)
			r := cli(t, baseEnv(dir), "doctor", "--reset")
			if r.code != 75 || !strings.Contains(r.stderr, "doctor --reset refused") || !strings.Contains(r.stderr, tc.want) {
				t.Errorf("doctor --reset: exit %d, want 75 naming %q (stderr %q)", r.code, tc.want, r.stderr)
			}
			if after := tree(t, dir); after != before {
				t.Errorf("a refused reset changed the directory:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func TestDoctorResetEmptiesAnIdleDirectory(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	c := openLib(t, dir)
	g := hold(t, c, "gate", map[string]coreslot.Range{"cpu": rng(2, 2)})
	g.Close() // nothing is alive any more; FORMAT, gen, a hold-less queue and the tokens remain
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordDegraded("test"); err != nil {
		t.Fatal(err)
	}
	if r := cli(t, baseEnv(dir), "run", "--need", "cpu=1", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	lockInode := inodeOf(t, filepath.Join(dir, "admit.lock"))
	tokenInode := inodeOf(t, filepath.Join(dir, "res", "cpu", "0"))

	r := cli(t, baseEnv(dir), "doctor", "--reset")
	if r.code != 0 || !strings.Contains(r.stdout, "reset: removed ") {
		t.Fatalf("doctor --reset: exit %d (stderr %q):\n%s", r.code, r.stderr, r.stdout)
	}
	for _, f := range []string{"FORMAT", "gen", "degraded"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", f, err)
		}
	}
	for _, d := range []string{"queue", "holds", "cancel", "log"} {
		if got := entries(t, dir, d); len(got) != 0 {
			t.Errorf("%s/ holds %v", d, got)
		}
	}
	for _, f := range []string{"admit.lock", "config.toml", "res"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if inodeOf(t, filepath.Join(dir, "admit.lock")) != lockInode || inodeOf(t, filepath.Join(dir, "res", "cpu", "0")) != tokenInode {
		t.Error("admit.lock or a token file was replaced")
	}
	if r := cli(t, baseEnv(dir), "run", "--need", "cpu=1", "--", "true"); r.code != 0 {
		t.Errorf("a run after the reset exited %d (stderr %q)", r.code, r.stderr)
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
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	c := openLib(t, dir)
	hold(t, c, "gate", map[string]coreslot.Range{"cpu": rng(4, 4)})
	a := queue(t, c, "twin", map[string]coreslot.Range{"cpu": rng(1, 1)})
	b := queue(t, c, "twin", map[string]coreslot.Range{"cpu": rng(1, 1)})
	solo := queue(t, c, "solo", map[string]coreslot.Range{"cpu": rng(1, 1)})

	r := cli(t, baseEnv(dir), "cancel", "--label", "twin")
	if r.code != 75 || !strings.Contains(r.stderr, "2 waiting tickets have the label twin") {
		t.Errorf("cancel --label twin: exit %d, want 75 naming the two (stderr %q)", r.code, r.stderr)
	}
	if got := entries(t, dir, "cancel"); len(got) != 0 {
		t.Errorf("an ambiguous label published cancel requests: %v", got)
	}
	for _, w := range []*coreslot.Waiter{a, b, solo} {
		if _, ok, err := w.Poll(); ok || err != nil {
			t.Errorf("a waiter was disturbed: admitted %v, %v", ok, err)
		}
	}

	if r := cli(t, baseEnv(dir), "cancel", "--label", "solo"); r.code != 0 {
		t.Fatalf("cancel --label solo: exit %d (stderr %q)", r.code, r.stderr)
	}
	if _, _, err := solo.Poll(); err != coreslot.ErrCancelled {
		t.Errorf("the cancelled waiter's Poll = %v, want ErrCancelled", err)
	}
	if _, ok, err := a.Poll(); ok || err != nil {
		t.Errorf("a twin was disturbed: %v, %v", ok, err)
	}
}

func TestCancelByIDAndRefusals(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	c := openLib(t, dir)
	hold(t, c, "gate", map[string]coreslot.Range{"cpu": rng(4, 4)})
	w := queue(t, c, "victim", map[string]coreslot.Range{"cpu": rng(1, 1)})

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
	if _, _, err := w.Poll(); err != coreslot.ErrCancelled {
		t.Errorf("Poll = %v, want ErrCancelled", err)
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
