//go:build coreslot_testhooks && unix

package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
)

const telemetrySwitch = "[telemetry]\nenabled = true\n"

// lifeOutcome is what one run of a row left: what the test compares, kept after its directory went.
type lifeOutcome struct {
	codes   []int
	holds   []string            // the live holds and tickets at each of the row's marks
	runKeys map[string][]string // per label, the keys of each run-log line
	events  []map[string]any    // run lines of events.jsonl, in file order
	files   []string            // everything under telemetry/
}

// code waits for p and records its exit status.
func (o *lifeOutcome) code(p *Proc) { o.codes = append(o.codes, p.Wait()) }

// mark records what the state directory holds now, for a moment both modes pass through.
func (o *lifeOutcome) mark(e Env) {
	s := e.Status()
	var now []string
	for _, h := range s.Holds {
		now = append(now, fmt.Sprintf("hold %s %d", h.Label, h.NeedMiB))
	}
	for _, tk := range s.Tickets {
		now = append(now, "ticket "+tk.Label)
	}
	slices.Sort(now)
	o.holds = append(o.holds, strings.Join(now, ","))
}

// names is the events of the run labelled label, space separated.
func (o *lifeOutcome) names(label string) string {
	var names []string
	for _, m := range o.ofLabel(label) {
		names = append(names, m["e"].(string))
	}
	return strings.Join(names, " ")
}

func (o *lifeOutcome) ofLabel(label string) []map[string]any {
	var out []map[string]any
	for _, m := range o.events {
		if m["label"] == label {
			out = append(out, m)
		}
	}
	return out
}

// wantField fails unless the first e of label has key equal to value.
func (o *lifeOutcome) wantField(t *testing.T, label, e, key string, value any) {
	t.Helper()
	for _, m := range o.ofLabel(label) {
		if m["e"] == e {
			if m[key] != value {
				t.Errorf("%s's %s: %s = %v, want %v (line %v)", label, e, key, m[key], value, m)
			}
			return
		}
	}
	t.Errorf("%s has no %s among %q", label, e, o.names(label))
}

// lifeRow is one run kind of the lifecycle: what it does, and the events each label must leave.
type lifeRow struct {
	name    string
	config  string
	notRoot bool
	want    map[string]string // label: a regular expression over its events, space separated
	trace   func(t *testing.T, e Env, o *lifeOutcome)
	check   func(t *testing.T, o *lifeOutcome)
}

// runOne runs a row's command, which holds until released, to its end.
func runOne(e Env, o *lifeOutcome, label string, flags ...string) {
	p := e.Run(label, append([]string{"--mem", "100M"}, flags...)...)
	e.WaitStarted(label)
	o.mark(e)
	e.ReleaseChild(label)
	o.code(p)
}

// waiter starts A, which fills the room, and B, once B has said that it waits.
func waiter(e Env, flags ...string) (a, b *Proc) {
	e.t.Helper()
	a = e.Run("A", "--mem", "1000M")
	e.WaitStarted("A")
	b = e.Run("B", append([]string{"--mem", "500M"}, flags...)...)
	e.Eventually(startWait, "B says it waits", func() bool { return strings.Contains(b.Stderr(), "waiting for") })
	return a, b
}

// finishA lets A end, so that its run-log line is there for the comparison.
func finishA(e Env, o *lifeOutcome, a *Proc) {
	e.ReleaseChild("A")
	o.code(a)
}

// stopRow starts A, the job no round stops, and B with flags, and stops B once from the floor.
func stopRow(t *testing.T, e Env, flags ...string) (a, b *Proc) {
	t.Helper()
	e.Learn("B")
	a = e.Run("A", "--mem", "100M")
	e.WaitStarted("A")
	e.WaitTicked("A")
	b = e.Run("B", append([]string{"--mem", "100M"}, flags...)...)
	stopAgain(t, e, b, 1024)
	return a, b
}

// nested is the shell text of a pass-through labelled inner whose command holds until released.
func nested(e Env) string {
	return fmt.Sprintf(`%q run --label inner --mem 64M -- %q hold %q inner`, bins.coreslot, bins.child, e.MarkDir)
}

// runScript starts a run labelled label whose command is sh -c script.
func runScript(e Env, label, script string, flags ...string) *Proc {
	args := append([]string{"run", "--restart=yes", "--label", label, "--mem", "100M"}, flags...)
	return e.Start(append(args, "--", "sh", "-c", script)...)
}

const foreignFormat = `printf 'coreslot-protocol 3\n' > "$CORESLOT_STATE_DIR/FORMAT"; `

// lifeRows is the lifecycle, row by row. Live lines come every 30 s, so most runs here have none.
var lifeRows = []lifeRow{
	{name: "admitted_at_once", config: roomConfig(1000), want: map[string]string{"R": `^enqueue admitted ended$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) { runOne(e, o, "R") }},
	{name: "waits_then_admitted", config: roomConfig(1000), want: map[string]string{"A": `^enqueue admitted ended$`, "B": `^enqueue( waiting)+ admitted ended$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			a, b := waiter(e)
			o.mark(e)
			finishA(e, o, a)
			e.WaitStarted("B")
			e.ReleaseChild("B")
			o.code(b)
		},
		check: func(t *testing.T, o *lifeOutcome) { o.wantField(t, "B", "waiting", "reason", "room_short") }},
	{name: "waits_on_the_memory_level", config: watchConfig(""), want: map[string]string{"B": `^enqueue( waiting)+ timeout$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			a := e.Run("A", "--mem", "100M")
			e.WaitStarted("A")
			e.WaitTicked("A")
			e.SetReadings(floor(1024))
			e.Eventually(startWait, "the level leaves green", func() bool { return e.Watch().Level >= watch.Yellow })
			b := e.Run("B", "--mem", "100M", "--timeout", "3s")
			o.code(b)
			finishA(e, o, a)
		},
		check: func(t *testing.T, o *lifeOutcome) { o.wantField(t, "B", "waiting", "reason", "watch_level") }},
	{name: "stopped_then_restarted", config: watchConfig(""), want: map[string]string{"B": `^enqueue( waiting)* admitted stopped ended enqueue( waiting)* admitted ended$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			a, b := stopRow(t, e)
			e.SetReadings(calm(2048))
			e.WaitStarted("B")
			e.ReleaseChild("B")
			o.code(b)
			finishA(e, o, a)
		},
		check: func(t *testing.T, o *lifeOutcome) {
			evs := o.ofLabel("B")
			last := evs[len(evs)-1]
			if last["attempt"] != 2.0 {
				t.Errorf("the restart's ended has attempt %v, want 2", last["attempt"])
			}
			o.wantField(t, "B", "ended", "attempt", 1.0)
			o.wantField(t, "B", "ended", "stopped_by", "watchdog_floor")
			o.wantField(t, "B", "stopped", "round", "floor")
			for _, m := range evs {
				if m["e"] == "enqueue" && m["attempt"] == 2.0 {
					if m["need_source"] != "restart_peak" {
						t.Errorf("the restart's enqueue: need_source %v", m["need_source"])
					}
					return
				}
			}
			t.Errorf("no enqueue with attempt 2 among %q", o.names("B"))
		}},
	{name: "gives_up_at_three_stops", config: watchConfig("post_kill_s = 0\n"),
		want: map[string]string{"B": `^(enqueue( waiting)* admitted stopped ended ){2}enqueue( waiting)* admitted stopped ended$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			a, b := stopRow(t, e)
			for k := int64(2); k <= 3; k++ {
				e.SetReadings(calm(2048 * (k - 1)))
				stopAgain(t, e, b, 2048*(k-1)+1024)
			}
			o.code(b)
			finishA(e, o, a)
		}},
	{name: "stopped_under_no_wait", config: watchConfig(""), want: map[string]string{"B": `^enqueue admitted stopped ended$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			a, b := stopRow(t, e, "--no-wait")
			o.code(b)
			finishA(e, o, a)
		}},
	{name: "no_wait_not_admitted", config: roomConfig(1000), want: map[string]string{"B": `^enqueue refused$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			a := e.Run("A", "--mem", "1000M")
			e.WaitStarted("A")
			o.code(e.Run("B", "--mem", "500M", "--no-wait"))
			finishA(e, o, a)
		},
		check: func(t *testing.T, o *lifeOutcome) { o.wantField(t, "B", "refused", "reason", "no_wait") }},
	{name: "foreign_format", config: roomConfig(1000), want: map[string]string{"R": `^$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			writeFormat(e, "3")
			runBlind(e, o)
		}},
	{name: "malformed_format", config: roomConfig(1000), want: map[string]string{"R": `^$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			writeFormat(e, "x")
			runBlind(e, o)
		}},
	{name: "config_cannot_be_read", config: "this is not toml\n", want: map[string]string{"R": `^$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) { runBlind(e, o) }},
	{name: "cancelled_while_waiting", config: roomConfig(1000), want: map[string]string{"B": `^enqueue( waiting)+ cancelled$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			a, b := waiter(e)
			e.Start("cancel", "--label", "B").ExitCode(0)
			o.code(b)
			finishA(e, o, a)
		}},
	{name: "timeout_fails", config: roomConfig(1000), want: map[string]string{"B": `^enqueue( waiting)+ timeout$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			a, b := waiter(e, "--timeout", "1s")
			o.code(b)
			finishA(e, o, a)
		},
		check: func(t *testing.T, o *lifeOutcome) { o.wantField(t, "B", "timeout", "on_timeout", "fail") }},
	{name: "timeout_in_a_restart", config: roomConfig(64 << 10),
		want: map[string]string{"B": `^enqueue( waiting)* admitted stopped ended enqueue( waiting)* timeout$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			a, b := stopRow(t, e, "--timeout", "5s", "--on-timeout", "run")
			o.code(b)
			finishA(e, o, a)
		}},
	{name: "timeout_runs_outside", config: roomConfig(1000), want: map[string]string{"B": `^enqueue( waiting)+ timeout outside( live)* ended$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			a, b := waiter(e, "--timeout", "1s", "--on-timeout", "run")
			e.WaitStarted("B")
			o.mark(e)
			e.ReleaseChild("B")
			o.code(b)
			finishA(e, o, a)
		},
		check: func(t *testing.T, o *lifeOutcome) { o.wantField(t, "B", "outside", "kind", "timeout_run") }},
	{name: "fail_open_with_a_hold", config: roomConfig(1000), notRoot: true, want: map[string]string{"R": `^outside( live)* ended$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			runOne(e, o, "setup")
			queue := filepath.Join(e.Dir, store.QueueDir)
			if err := os.Chmod(queue, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(queue, 0o700) })
			runOne(e, o, "R")
		},
		check: func(t *testing.T, o *lifeOutcome) {
			o.wantField(t, "R", "outside", "kind", "fail_open")
			o.wantField(t, "R", "outside", "reason", "state_error")
			o.wantField(t, "R", "ended", "fail_open", "state_error")
		}},
	{name: "pass_through", config: roomConfig(1000), want: map[string]string{"outer": `^enqueue admitted ended$`, "inner": `^outside ended$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			p := runScript(e, "outer", nested(e))
			e.WaitStarted("inner")
			o.mark(e)
			e.ReleaseChild("inner")
			o.code(p)
		},
		check: func(t *testing.T, o *lifeOutcome) { o.wantField(t, "inner", "outside", "kind", "pass_through") }},
	{name: "pass_through_killed_with_its_parent", config: watchConfig(""), want: map[string]string{"B": `^enqueue admitted stopped ended$`, "inner": `^outside$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			e.Learn("B")
			a := e.Run("A", "--mem", "100M")
			e.WaitStarted("A")
			e.WaitTicked("A")
			b := runScript(e, "B", nested(e), "--no-wait")
			kid := e.WaitStarted("inner")
			e.WaitTicked("B")
			e.SetReadings(floor(1024))
			e.Eventually(startWait, "the nested command is killed", func() bool { return !alive(kid.PID) })
			o.code(b)
			finishA(e, o, a)
		}},
	{name: "format_changes_under_a_pass_through", config: roomConfig(1000), want: map[string]string{"outer": `^enqueue admitted$`, "inner": `^$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			p := runScript(e, "outer", foreignFormat+nested(e))
			e.WaitStarted("inner")
			e.ReleaseChild("inner")
			o.code(p)
		}},
	{name: "format_changes_under_a_run", config: roomConfig(1000), want: map[string]string{"R": `^enqueue admitted$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) { o.code(runScript(e, "R", foreignFormat)) }},
	{name: "signal_while_waiting", config: roomConfig(1000), want: map[string]string{"B": `^enqueue( waiting)+ interrupted$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			a, b := waiter(e)
			b.signal(b.pid, syscall.SIGTERM)
			o.code(b)
			finishA(e, o, a)
		},
		check: func(t *testing.T, o *lifeOutcome) { o.wantField(t, "B", "interrupted", "signal", "SIGTERM") }},
	{name: "exec_fails", config: roomConfig(1000), want: map[string]string{"R": `^enqueue admitted not_started$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			o.code(e.Start("run", "--restart=yes", "--label", "R", "--mem", "100M", "--", filepath.Join(e.MarkDir, "no-such-command")))
		},
		check: func(t *testing.T, o *lifeOutcome) { o.wantField(t, "R", "not_started", "reason", "exec") }},
	{name: "reset_keeps_it_as_the_run_log", config: roomConfig(1000), want: map[string]string{"R": `^enqueue admitted ended$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			runOne(e, o, "R")
			e.Start("doctor", "--reset").ExitCode(0)
		}},
	{name: "reset_of_another_format_removes_it", config: roomConfig(1000), want: map[string]string{"R": `^$`},
		trace: func(t *testing.T, e Env, o *lifeOutcome) {
			runOne(e, o, "R")
			writeFormat(e, "3")
			e.Start("doctor", "--reset").ExitCode(0)
		},
		check: func(t *testing.T, o *lifeOutcome) {
			if len(o.files) != 0 {
				t.Errorf("after a reset, under telemetry/: %q", o.files)
			}
		}},
}

// runBlind is runOne for a directory Status cannot read: it takes no mark.
func runBlind(e Env, o *lifeOutcome) {
	p := e.Run("R", "--mem", "100M")
	e.WaitStarted("R")
	e.ReleaseChild("R")
	o.code(p)
}

// lifeDone keeps each row's outcome per mode: the tests compare the same runs.
var lifeDone sync.Map

// outcome runs row with telemetry on or off, once per test binary.
func (row lifeRow) outcome(t *testing.T, on bool) *lifeOutcome {
	t.Helper()
	key := fmt.Sprint(row.name, on)
	if o, ok := lifeDone.Load(key); ok {
		return o.(*lifeOutcome)
	}
	if row.notRoot && os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	e := State(t).on(t)
	cfg := row.config
	if on {
		cfg += telemetrySwitch
	}
	e.WriteConfig(cfg)
	o := &lifeOutcome{runKeys: map[string][]string{}}
	row.trace(t, e, o)
	o.read(t, e)
	if keep := os.Getenv("CORESLOT_TELEMETRY_KEEP"); keep != "" && on {
		for _, d := range []string{store.TelemetryDir, store.LogDir} {
			if err := os.CopyFS(filepath.Join(keep, row.name, d), os.DirFS(filepath.Join(e.Dir, d))); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Error(err)
			}
		}
	}
	lifeDone.Store(key, o)
	return o
}

// varyingRunKeys are run-log keys that come and go with timing: a tick's share and the sampled peak.
var varyingRunKeys = []string{"tick_share", "mem_peak_kib"}

// read collects what the row left in the state directory.
func (o *lifeOutcome) read(t *testing.T, e Env) {
	t.Helper()
	root := filepath.Join(e.Dir, store.TelemetryDir)
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			o.files = append(o.files, p)
		}
		return nil
	}); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for _, m := range jsonLines(t, filepath.Join(root, store.EventsFile)) {
		if m["e"] != "memory" && m["e"] != "containers" { // on no run's order
			o.events = append(o.events, m)
		}
	}
	for _, m := range jsonLines(t, filepath.Join(e.Dir, store.LogDir, store.RunLog)) {
		for _, k := range varyingRunKeys {
			delete(m, k)
		}
		label, _ := m["label"].(string)
		o.runKeys[label] = append(o.runKeys[label], strings.Join(slices.Sorted(maps.Keys(m)), ","))
	}
}

// jsonLines decodes a JSON-lines file; a missing one is empty.
func jsonLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("%s: line %q: %v", path, l, err)
		}
		out = append(out, m)
	}
	return out
}

// Each run kind leaves its events in the order the lifecycle gives. Mutations: the Stops at the
// hold update and the run-log append, both gone, redden format_changes_under_a_run; the
// pass-through's FORMAT read reddens format_changes_under_a_pass_through; fail-open making no
// recorder reddens fail_open_with_a_hold; attempt left at 1 reddens stopped_then_restarted.
func TestTelemetryLifecycle(t *testing.T) {
	for _, row := range lifeRows {
		t.Run(row.name, func(t *testing.T) {
			o := row.outcome(t, true)
			for label, want := range row.want {
				if got := o.names(label); !regexp.MustCompile(want).MatchString(got) {
					t.Errorf("%s: events %q, want %s", label, got, want)
				}
			}
			if row.check != nil {
				row.check(t, o)
			}
		})
	}
}

// With the switch off no row leaves a file under telemetry/. Mutation: a recorder made whatever
// the switch says reddens the first row.
func TestTelemetryOffWritesNothing(t *testing.T) {
	for _, row := range lifeRows {
		t.Run(row.name, func(t *testing.T) {
			if o := row.outcome(t, false); len(o.files) != 0 {
				t.Errorf("with telemetry off, under telemetry/: %q", o.files)
			}
		})
	}
}

// Recording changes no outcome: exit codes, what was held at each mark and the run log's keys are
// the same on and off. Mutation: an extra key on the run-log record when a recorder is live; a
// run that exits by the recorder's say reddens the codes.
func TestTelemetryChangesNoOutcome(t *testing.T) {
	for _, row := range lifeRows {
		t.Run(row.name, func(t *testing.T) {
			on, off := row.outcome(t, true), row.outcome(t, false)
			if !slices.Equal(on.codes, off.codes) {
				t.Errorf("exit codes on %v, off %v", on.codes, off.codes)
			}
			if !slices.Equal(on.holds, off.holds) {
				t.Errorf("holds on %q, off %q", on.holds, off.holds)
			}
			if !maps.EqualFunc(on.runKeys, off.runKeys, slices.Equal[[]string]) {
				t.Errorf("run-log keys on %q, off %q", on.runKeys, off.runKeys)
			}
		})
	}
}

// Every line carries what its event promises. live and kept need 30 s of a run and a round that
// spares it: their own unit tests cover them. Mutations: a key dropped from a line reddens its
// row; a new event with no row here reddens "not in the table".
func TestTelemetryKeySets(t *testing.T) {
	waitEnd := []string{"waited_s", "reason_s", "last_reason"}
	required := map[string][]string{
		"enqueue":     {"ticket", "need_mib", "need_source", "real_memory", "typical_s", "oversized", "timeout_s", "on_timeout", "version", "mem_mib", "mode", "reserve_mib", "watch_config"},
		"waiting":     {"reason", "reason_s", "evals", "max_gap_ms", "headroom_mib", "headroom_ok", "reserve_mib", "room_mib", "holders"},
		"admitted":    {"hold", "waited_s", "reason_s", "last_reason", "headroom_mib", "headroom_ok", "reserve_mib", "room_mib", "watch_gate"},
		"ended":       {"record"},
		"not_started": {"reason"},
		"stopped":     {"round", "observe", "level", "excess", "list_pos"},
		"outside":     {"kind", "reason", "need_mib"},
		"interrupted": append([]string{"signal"}, waitEnd...),
		"cancelled":   waitEnd,
		"refused":     append([]string{"reason"}, waitEnd...),
		"timeout":     append([]string{"on_timeout"}, waitEnd...),
	}
	common := []string{"v", "e", "time_ns", "up_ns", "awake_ns", "lost", "run", "attempt", "n", "label"}
	seen := map[string]bool{}
	for _, row := range lifeRows {
		for _, m := range row.outcome(t, true).events {
			name := m["e"].(string)
			seen[name] = true
			keys, ok := required[name]
			if !ok {
				t.Errorf("%s: event %s is not in the table", row.name, name)
			}
			for _, k := range append(slices.Clone(common), keys...) {
				if _, ok := m[k]; !ok {
					t.Errorf("%s: %s lacks %s: %v", row.name, name, k, m)
				}
			}
		}
	}
	for name := range required {
		if !seen[name] {
			t.Errorf("no row wrote %s", name)
		}
	}
}
