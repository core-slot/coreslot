//go:build unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// trimIn runs trimCommand in this process against dir, so that a test can shorten its wait.
func trimIn(t *testing.T, dir string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv("CORESLOT_STATE_DIR", dir)
	stdout, stderr = capture(t, func(io IO) { code = trimCommand(args, io) })
	return code, stdout, stderr
}

// shortTrimWait makes trimCommand wait d for the run's answer until the test ends.
func shortTrimWait(t *testing.T, d time.Duration) {
	t.Helper()
	saved := trimAnswerWait
	trimAnswerWait = d
	t.Cleanup(func() { trimAnswerWait = saved })
}

// holdIDOf is the id `status` prints for g's hold.
func holdIDOf(t *testing.T, g *engine.Grant) string {
	t.Helper()
	id, ok := wire.ParseName(g.HoldName())
	if !ok {
		t.Fatalf("hold name %q", g.HoldName())
	}
	return id.String()
}

// answerTrim stands in for g's run: once trim.json names g's hold it applies f to the hold's body,
// as a run does at its tick. It gives up when the test ends.
func answerTrim(t *testing.T, g *engine.Grant, f func(h *wire.Hold)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if _, ok := g.TrimRequested(); ok {
				if err := g.UpdateHold(f); err != nil {
					t.Errorf("answering the trim: %v", err)
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

func trimFileExists(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, store.TrimFile))
	return err == nil
}

// trim takes exactly one hold, by id or by --label; anything else is a usage error that prints the
// usage. Mutation: accept both an id and --label → exit 75 or 0 for that case, red.
func TestTrimUsage(t *testing.T) {
	dir := budgetDir(t, 512)
	id := wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String()
	for _, args := range [][]string{{"trim"}, {"trim", id, "--label", "x"}, {"trim", "--label", "x", id}, {"trim", id, id}, {"trim", "not-an-id"}} {
		r := cli(t, baseEnv(dir), args...)
		if r.code != 64 {
			t.Errorf("%q: exit %d, want 64 (stderr %q)", args, r.code, r.stderr)
		}
		if args[len(args)-1] != "not-an-id" && !strings.Contains(r.stderr, trimUsage) {
			t.Errorf("%q: stderr %q, want the usage", args, r.stderr)
		}
	}
	if r := cli(t, baseEnv(dir), "trim", "--help"); r.code != 0 || !strings.Contains(r.stdout, trimUsage) {
		t.Errorf("trim --help: exit %d, stdout %q; want 0 and the usage", r.code, r.stdout)
	}
	if r := cli(t, baseEnv(dir), "help"); !strings.Contains(r.stdout, "\n  trim ") {
		t.Errorf("the usage does not list trim:\n%s", r.stdout)
	}
}

// Each request that could do nothing is refused with exit 75 and its own sentence, and none writes
// trim.json. Mutation: write the request before the engine's checks (TrimContext) → trim.json
// exists after each refusal, red.
func TestTrimRefusals(t *testing.T) {
	t.Run("no state directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "never ran")
		r := cli(t, baseEnv(dir), "trim", wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String())
		if r.code != 75 || !strings.Contains(r.stderr, "no state directory yet (nothing has run)") {
			t.Errorf("exit %d, stderr %q; want 75 and no state directory", r.code, r.stderr)
		}
		if _, err := os.Lstat(dir); err == nil {
			t.Error("trim created the state directory")
		}
	})

	dir := budgetDir(t, 4096)
	c := openLib(t, dir)
	live := hold(t, c, "live", 512)
	exited := hold(t, c, "exited", 512)
	if err := exited.UpdateHold(func(h *wire.Hold) { h.ChildExited = true }); err != nil {
		t.Fatal(err)
	}
	open, err := c.FailOpen(spec.Request{Label: "open", NeedMiB: 256}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { open.Close() })
	ids := map[string]string{} // status exits 3: the hold outside the budget recorded degraded mode
	for _, h := range statusDoc(t, dir, 3)["holds"].([]any) {
		h := h.(map[string]any)
		ids[h["label"].(string)] = h["id"].(string)
	}
	if ids["live"] != holdIDOf(t, live) {
		t.Fatalf("status gives the hold the id %q, want %q", ids["live"], holdIDOf(t, live))
	}
	for _, c := range []struct{ name, id, want string }{
		{"no such hold", wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String(), "coreslot: trim: no live hold " + wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String() + " (it has ended, or the id is not a hold's)\n"},
		{"nothing reserved", ids["open"], "coreslot: trim: open reserves nothing (it runs outside the budget), so there is nothing to lower\n"},
		{"command exited", ids["exited"], "coreslot: trim: the command of exited has exited; its run is releasing the hold\n"},
	} {
		r := cli(t, baseEnv(dir), "trim", c.id)
		if r.code != 75 || r.stderr != c.want {
			t.Errorf("%s: exit %d, stderr %q; want 75 and %q", c.name, r.code, r.stderr, c.want)
		}
		if trimFileExists(dir) {
			t.Fatalf("%s: a refused trim wrote trim.json", c.name)
		}
	}
	if r := cli(t, baseEnv(dir), "trim", "--label", "nobody"); r.code != 75 || r.stderr != "coreslot: trim: no live hold reserving memory has the label nobody\n" {
		t.Errorf("--label nobody: exit %d, stderr %q; want 75", r.code, r.stderr)
	}

	t.Run("another format", func(t *testing.T) {
		dir := stateDir(t, "")
		if err := os.WriteFile(filepath.Join(dir, "FORMAT"), []byte("coreslot-protocol 3\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := cli(t, baseEnv(dir), "trim", wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String())
		want := "coreslot: trim refused: state format 3 is not supported; run coreslot doctor --reset\n"
		if r.code != 75 || r.stderr != want {
			t.Errorf("exit %d, stderr %q; want 75 and %q", r.code, r.stderr, want)
		}
		if trimFileExists(dir) {
			t.Error("trim under another format wrote trim.json")
		}
	})

	t.Run("section busy", func(t *testing.T) {
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
		r := cli(t, baseEnv(dir), "trim", wire.ID{OrderKey: 1, PID: 2, Seq: 3}.String())
		pid := fmt.Sprintf("pid %d", os.Getpid())
		if r.code != 75 || !strings.HasPrefix(r.stderr, "coreslot: trim refused: the admission section stayed held for 1s") ||
			!strings.Contains(r.stderr, pid) || !strings.HasSuffix(r.stderr, "; no request was written\n") {
			t.Errorf("exit %d, stderr %q; want 75, the section held by %s", r.code, r.stderr, pid)
		}
		if trimFileExists(dir) {
			t.Error("trim with the section held wrote trim.json")
		}
	})
}

// A label names one live hold that reserves memory; several are a refusal that lists their ids,
// and a hold outside the budget with that label is not one of them. Mutation: count the holds
// that reserve nothing → "2 live holds" for solo, red.
func TestTrimByLabelAmbiguous(t *testing.T) {
	shortTrimWait(t, 300*time.Millisecond)
	dir := budgetDir(t, 4096)
	c := openLib(t, dir)
	a, b := hold(t, c, "twin", 512), hold(t, c, "twin", 512)
	solo := hold(t, c, "solo", 512)
	open, err := c.FailOpen(spec.Request{Label: "solo", NeedMiB: 256}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { open.Close() })

	code, _, stderr := trimIn(t, dir, "--label", "twin")
	ids := []string{holdIDOf(t, a), holdIDOf(t, b)}
	if ids[0] > ids[1] {
		ids[0], ids[1] = ids[1], ids[0]
	}
	want := "coreslot: trim: 2 live holds have the label twin; trim one by id: " + ids[0] + ", " + ids[1] + "\n"
	if code != 75 || stderr != want {
		t.Errorf("--label twin: exit %d, stderr %q; want 75 and %q", code, stderr, want)
	}
	if trimFileExists(dir) {
		t.Fatal("an ambiguous label wrote trim.json")
	}
	if code, _, stderr := trimIn(t, dir, "--label", "solo"); code != 0 {
		t.Fatalf("--label solo: exit %d (stderr %q)", code, stderr)
	}
	if _, ok := solo.TrimRequested(); !ok {
		t.Error("--label solo wrote no request for solo's hold")
	}
	if _, ok := a.TrimRequested(); ok {
		t.Error("--label solo asked a twin to trim")
	}
}

// The command says what the run answered: lowered; following with nothing released yet, and why;
// already following; or no answer, which is not a refusal. A container hold adds what is not
// measured. Mutation: take a lowered need_mib as the answer instead of trimmed → the case that
// sets trimmed alone gets "has not answered yet", red.
func TestTrimAnswers(t *testing.T) {
	const asked = 4000
	lowered := func(h *wire.Hold) { h.Trimmed, h.AskedMiB, h.NeedMiB, h.FootprintMiB = true, asked, 600, 200 }
	following := func(h *wire.Hold) { h.Trimmed, h.AskedMiB, h.FootprintMiB = true, asked, 3000 }
	cases := []struct {
		name      string
		config    string // [watch] in the configuration
		age       time.Duration
		container bool
		before    func(h *wire.Hold) // the body before the command
		answer    func(h *wire.Hold) // the run's answer; nil for none
		want      []string           // the lines printed, quoted, but for a regular expression's \d+
	}{
		{name: "lowered", answer: lowered,
			want: []string{"coreslot: x reserves 600 MiB now (asked 3.9 GiB) and follows what it uses"}},
		{name: "young", answer: following,
			want: []string{"coreslot: x follows what it uses from now on; nothing is released yet: it started \\d+s ago, and its reservation follows from its first minute on"}},
		{name: "old", config: "trim_window_s = 2\n", age: 2500 * time.Millisecond, answer: following,
			want: []string{"coreslot: x follows what it uses from now on; nothing is released yet: it used 2.9 GiB within the last 2s"}},
		{name: "no answer",
			want: []string{"coreslot: x: asked; the run has not answered yet. A run started by an older coreslot never will; see coreslot status"}},
		{name: "already", before: lowered,
			want: []string{"coreslot: x already follows what it uses (reserves 600 MiB, asked 3.9 GiB)"}},
		{name: "container", container: true, answer: lowered,
			want: []string{"coreslot: x reserves 600 MiB now (asked 3.9 GiB) and follows what it uses",
				"coreslot: its containers' memory is not measured, so its reservation follows the client's own memory"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			shortTrimWait(t, 1500*time.Millisecond)
			dir := budgetDir(t, 16384)
			writeConfig(t, dir, "[memory]\nreserve = \"0\"\n[watch]\n"+c.config)
			g := hold(t, openLib(t, dir), "x", asked)
			if err := g.UpdateHold(func(h *wire.Hold) {
				h.Container = c.container
				if c.before != nil {
					c.before(h)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if c.answer != nil {
				answerTrim(t, g, c.answer)
			}
			time.Sleep(c.age)
			code, stdout, stderr := trimIn(t, dir, holdIDOf(t, g))
			want := regexp.QuoteMeta(strings.Join(c.want, "\n") + "\n")
			want = strings.ReplaceAll(want, `\\d\+`, `\d+`)
			if code != 0 || !regexp.MustCompile("^"+want+"$").MatchString(stdout) {
				t.Errorf("exit %d, stdout %q (stderr %q); want 0 and %q", code, stdout, stderr, want)
			}
		})
	}
}

// A hold's line ends with its id, as a ticket's does; a trimmed one shows what it asked for and
// says it was trimmed. The document carries id, asked_mib (need_mib when the body has none) and
// trimmed on every hold. Mutations: print asked_mib as the body's alone → 0 for b, red; drop the
// trimmed word → red.
func TestStatusShowsHoldIDAskedTrimmed(t *testing.T) {
	rep := memReport()
	rep.Holds = []engine.HolderInfo{
		{Name: "n1", ID: "id-a", Label: "a", PID: 7, NeedMiB: 600, AskedMiB: 4000, Trimmed: true},
		{Name: "n2", ID: "id-b", Label: "b", PID: 8, NeedMiB: 512},
	}
	h := buildStatus(rep, 0, nil, 0).human()
	for _, want := range []string{"\n  a  pid 7  mem 600 MiB (asked 3.9 GiB)  held ", "  trimmed  id id-a\n", "\n  b  pid 8  mem 512 MiB  held ", "  id id-b\n"} {
		if !strings.Contains(h, want) {
			t.Errorf("status lacks %q:\n%s", want, h)
		}
	}
	if strings.Count(h, "asked") != 1 || strings.Count(h, "trimmed") != 1 {
		t.Errorf("status shows asked or trimmed on b:\n%s", h)
	}
	var doc struct {
		Holds []struct {
			ID       string `json:"id"`
			AskedMiB *int64 `json:"asked_mib"`
			Trimmed  *bool  `json:"trimmed"`
		} `json:"holds"`
	}
	if err := json.Unmarshal([]byte(statusJSON(t, rep)), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Holds) != 2 {
		t.Fatalf("holds %+v", doc.Holds)
	}
	for i, want := range []struct {
		id      string
		asked   int64
		trimmed bool
	}{{"id-a", 4000, true}, {"id-b", 512, false}} {
		got := doc.Holds[i]
		if got.ID != want.id || got.AskedMiB == nil || *got.AskedMiB != want.asked || got.Trimmed == nil || *got.Trimmed != want.trimmed {
			t.Errorf("hold %d: %s; want id %q asked_mib %d trimmed %v", i, statusJSON(t, rep), want.id, want.asked, want.trimmed)
		}
	}
}
