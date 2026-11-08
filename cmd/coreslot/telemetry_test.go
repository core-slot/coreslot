//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/telemetry"
	"github.com/core-slot/coreslot/internal/wire"
)

const telemetryOn = "[memory]\nreserve = \"0\"\n[telemetry]\nenabled = true\n"

// writeConfig replaces the state directory's configuration.
func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// recordedDir is a budgetDir whose layout exists, with telemetry on from now on: the run that
// made the layout left no events.
func recordedDir(t *testing.T, roomMiB int64) string {
	t.Helper()
	dir := budgetDir(t, roomMiB)
	if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	writeConfig(t, dir, telemetryOn)
	return dir
}

func hasTelemetryDir(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, store.TelemetryDir))
	return err == nil
}

// events decodes the state directory's events.jsonl.
func events(t *testing.T, dir string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, store.TelemetryDir, store.EventsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func eventNames(evs []map[string]any) string {
	names := make([]string, 0, len(evs))
	for _, m := range evs {
		names = append(names, m["e"].(string))
	}
	return strings.Join(names, " ")
}

// event is the first line of event e, failing when there is none.
func event(t *testing.T, evs []map[string]any, e string) map[string]any {
	t.Helper()
	for _, m := range evs {
		if m["e"] == e {
			return m
		}
	}
	t.Fatalf("no %s among %q", e, eventNames(evs))
	return nil
}

// bed is an in-process run with telemetry on, over a state directory whose machine has room MiB
// of headroom and no reserve.
type bed struct {
	dir string
	e   *engine.Engine
	r   *run
}

func newBed(t *testing.T, roomMiB int64) *bed {
	t.Helper()
	dir := stateDir(t, telemetryOn)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.toml")
	e := engine.New(st, cfg, func() (config.Machine, error) {
		return config.Machine{MemMiB: 32768, HeadroomMiB: roomMiB, HeadroomOK: true}, nil
	}, engine.Clock{})
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stderr.Close() })
	r := &run{o: runOptions{need: 128, needSource: "flag", label: "go test", onTimeout: "fail", refusalCode: exitRefusal,
		argv: []string{"true"}}, io: IO{Stderr: stderr}, loc: location{dir: dir, cfgPath: cfg}, start: time.Now(),
		telemetry: true, memMiB: 32768}
	return &bed{dir: dir, e: e, r: r}
}

// wait runs the bed's wait with ctx and returns its outcome.
func (b *bed) wait(t *testing.T, ctx context.Context) (*engine.Grant, int, bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	return b.r.wait(ctx, cancel, b.e, b.r.o.request())
}

// admit runs the bed's wait to its grant and closes it.
func (b *bed) admit(t *testing.T) {
	t.Helper()
	g, code, ok := b.wait(t, context.Background())
	if !ok {
		t.Fatalf("not admitted: code %d, stderr %q", code, b.notices(t))
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
}

// notices is what the run has written to its stderr.
func (b *bed) notices(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(b.r.io.Stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// signalAfterFirstLine ends the bed's wait with a signal once it has written a waiting line.
func (b *bed) signalAfterFirstLine(t *testing.T) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pw.Close(); pr.Close() })
	b.r.io.Stderr = pw
	go func() {
		buf := make([]byte, 4096)
		if _, err := pr.Read(buf); err != nil {
			return
		}
		runSignals <- syscall.SIGTERM
		for {
			if _, err := pr.Read(buf); err != nil {
				return
			}
		}
	}()
}

// holdUntil publishes a hold reserving needMiB, live until the function it returns is called.
func holdUntil(t *testing.T, dir string, needMiB int64) (release func()) {
	t.Helper()
	f, err := os.Open(deadHold(t, dir, needMiB))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return func() { f.Close() }
}

// replaceClocks makes the recorder read fn; the test's end restores it.
func replaceClocks(t *testing.T, fn func() (int64, int64)) {
	t.Helper()
	saved := telemetry.Clocks
	telemetry.Clocks = fn
	t.Cleanup(func() { telemetry.Clocks = saved })
}

// Each way a need is resolved names itself, and a run's enqueue says which. Mutation: a source
// assigned to the wrong branch of resolveNeed reddens its row.
func TestEnqueueLine(t *testing.T) {
	lookup, _ := lookupOf(t, "[needs.\"pin\"]\nmem = \"2G\"\n")
	learned := func() (int64, bool) { return 31, true }
	for _, c := range []struct {
		args    []string
		learned func() (int64, bool)
		want    string
	}{
		{[]string{"--mem", "1G", "--", "true"}, noLearned, "flag"},
		{[]string{"--label", "pin", "--", "true"}, noLearned, "pin"},
		{[]string{"--label", "x", "--", "true"}, learned, "learned"},
		{[]string{"--", "docker", "run"}, noLearned, "container"},
		{[]string{"--label", "x", "--", "make"}, noLearned, "cold"},
	} {
		o, err := parseRun(c.args, IO{})
		if err != nil {
			t.Fatal(err)
		}
		o.resolveNeed(lookup, c.learned, 32768)
		if o.needSource != c.want {
			t.Errorf("%q: source %q, want %q", c.args, o.needSource, c.want)
		}
	}

	b := newBed(t, 1024)
	b.r.o.timeout, b.r.o.typicalS, b.r.oversized = 90*time.Second, 12, true
	b.admit(t)
	evs := events(t, b.dir)
	if eventNames(evs) != "enqueue admitted" {
		t.Fatalf("events %q, want enqueue admitted", eventNames(evs))
	}
	m := evs[0]
	if _, ok := wire.ParseName(fmt.Sprint(m["ticket"])); !ok || !strings.HasSuffix(fmt.Sprint(m["ticket"]), "go_test") {
		t.Errorf("ticket %v, want the queue's name of this run", m["ticket"])
	}
	for k, want := range map[string]any{"need_mib": 128.0, "need_source": "flag", "real_memory": false, "typical_s": 12.0,
		"oversized": true, "timeout_s": 90.0, "on_timeout": "fail", "version": cliVersion(), "mem_mib": 32768.0,
		"mode": "enforce", "reserve_mib": 0.0, "attempt": 1.0, "label": "go test"} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	if _, ok := m["need_before_mib"]; ok {
		t.Errorf("a first attempt has need_before_mib: %v", m)
	}
	wc, _ := m["watch_config"].(map[string]any)
	for _, k := range []string{"yellow_points", "red_points", "red_samples", "gate_pages_s", "floor_mib", "calm_s", "post_kill_s", "spacing_s"} {
		if _, ok := wc[k]; !ok {
			t.Errorf("watch_config lacks %s: %v", k, m["watch_config"])
		}
	}
}

// A need above what the machine can ever give says so on its enqueue. Mutation: ordinary not
// keeping Oversized's answer leaves false.
func TestEnqueueSaysOversized(t *testing.T) {
	dir := recordedDir(t, 512)
	if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--mem", "999999G", "--", "true"); r.code != 0 {
		t.Fatalf("exit %d: %q", r.code, r.stderr)
	}
	if m := event(t, events(t, dir), "enqueue"); m["oversized"] != true {
		t.Errorf("oversized %v, want true", m["oversized"])
	}
}

// A restart's enqueue is attempt n+1, with its peak as its need and what the need was before.
// Mutation: requeue leaving needSource alone reads flag; a stale attempt reads 1.
func TestEnqueueOfARestart(t *testing.T) {
	b := newBed(t, 1024)
	req := b.r.o.request()
	first, _, ok := b.r.wait(context.Background(), func() {}, b.e, req)
	if !ok {
		t.Fatalf("not admitted: %q", b.notices(t))
	}
	b.r.peakKiB, b.r.stops = 300<<10, 1
	b.r.requeue(&req, first)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	holdUntil(t, b.dir, 100) // with a hold live a restart waits for calm
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	b.r.o.timeout = 50 * time.Millisecond
	if _, _, ok := b.r.wait(ctx, cancel, b.e, req); ok {
		t.Fatal("a restart with no calm was admitted")
	}
	evs := events(t, b.dir)
	var m map[string]any
	for _, l := range evs {
		if l["e"] == "enqueue" {
			m = l
		}
	}
	if m["attempt"] != 2.0 || m["need_source"] != "restart_peak" || m["need_mib"] != 300.0 || m["need_before_mib"] != 128.0 {
		t.Errorf("the restart's enqueue: %v", m)
	}
	if evs[0]["run"] != m["run"] {
		t.Errorf("the run's id changed across attempts: %v, %v", evs[0]["run"], m["run"])
	}
}

// An enqueue that fails records nothing, and creates nothing. Mutation: making the recorder and
// writing the line before EnqueueContext's result is looked at.
func TestEnqueueFailureRecordsNothing(t *testing.T) {
	t.Run("the context ended", func(t *testing.T) {
		b := newBed(t, 1024)
		b.r.o.strict = true
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, ok := b.wait(t, ctx); ok {
			t.Fatal("admitted")
		}
		if hasTelemetryDir(b.dir) {
			t.Errorf("telemetry/ exists after a failed enqueue: %v", events(t, b.dir))
		}
		if b.r.rec != nil {
			t.Error("a recorder exists after a failed enqueue")
		}
	})
	t.Run("a foreign format", func(t *testing.T) {
		b := newBed(t, 1024)
		b.r.o.strict = true
		if err := os.WriteFile(filepath.Join(b.dir, "FORMAT"), []byte("99\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := b.wait(t, context.Background()); ok {
			t.Fatal("admitted")
		}
		if hasTelemetryDir(b.dir) {
			t.Errorf("telemetry/ exists after a refused enqueue: %v", events(t, b.dir))
		}
	})
}

// A waiter that is not the head has a reading too: the line's room and holders come from the
// waiting line's Snapshot. Mutation: the why closure not storing the report leaves room_mib out.
func TestWaitingLineFromSnapshot(t *testing.T) {
	b := newBed(t, 1000)
	holdUntil(t, b.dir, 600)
	ahead, err := b.e.EnqueueContext(context.Background(), spec.Request{Label: "ahead", NeedMiB: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer ahead.Withdraw()
	b.signalAfterFirstLine(t)
	if _, _, ok := b.wait(t, context.Background()); ok {
		t.Fatal("admitted")
	}
	m := event(t, events(t, b.dir), "waiting")
	for k, want := range map[string]any{"reason": "behind_ticket", "ahead": ahead.Name(), "room_mib": 400.0, "headroom_mib": 1000.0,
		"headroom_ok": true, "reserve_mib": 0.0} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	if _, ok := m["level"]; ok {
		t.Errorf("a level with no sample: %v", m["level"])
	}
	for _, k := range []string{"reason_s", "evals", "max_gap_ms"} {
		if _, ok := m[k]; !ok {
			t.Errorf("no %s: %v", k, m)
		}
	}
	holders, _ := m["holders"].([]any)
	if len(holders) != 1 {
		t.Fatalf("holders %v, want the one live hold", m["holders"])
	}
	h := holders[0].(map[string]any)
	if h["need_mib"] != 600.0 || h["footprint_mib"] != 0.0 || h["child_exited"] != false || h["owner_exited"] != true || !strings.HasSuffix(fmt.Sprint(h["hold"]), "-other") {
		t.Errorf("holder %v", h)
	}
	if _, ok := h["sample_age_ms"]; ok {
		t.Errorf("a hold never sampled has an age: %v", h)
	}
}

// A container waiter's room is the one its waiting line shows: it ignores the reservations.
// Mutation: room_mib from FreeMiB alone reads 400.
func TestWaitingRoomOfContainer(t *testing.T) {
	b := newBed(t, 1000)
	holdUntil(t, b.dir, 600)
	ahead, err := b.e.EnqueueContext(context.Background(), spec.Request{Label: "ahead", NeedMiB: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer ahead.Withdraw()
	b.r.o.argv, b.r.o.need = []string{"docker", "run"}, 2048
	b.signalAfterFirstLine(t)
	if _, _, ok := b.wait(t, context.Background()); ok {
		t.Fatal("admitted")
	}
	if m := event(t, events(t, b.dir), "waiting"); m["room_mib"] != 1000.0 {
		t.Errorf("room_mib %v, want the real free memory, 1000", m["room_mib"])
	}
}

// scriptedStepper answers its steps in order; each advances the clock by the step's delay first.
type scriptedStepper struct {
	clk   *fakeClock
	steps []scriptedStep
	i     int
}

type scriptedStep struct {
	after  time.Duration
	reason engine.WaitReason
}

func (s *scriptedStepper) due() time.Duration { return time.Second }

func (s *scriptedStepper) step(context.Context) (engine.StepResult, error) {
	st := s.steps[s.i]
	s.i++
	s.clk.now += st.after
	return engine.Waiting{Outcome: engine.Outcome{Reason: st.reason}}, nil
}

// The time between two steps is the first step's reason's: that is what the run waited on
// meanwhile. take clears. Mutation: charging the step's own reason gives room_short 2, behind_head 3.
func TestReasonSecondsAddUp(t *testing.T) {
	clk := &fakeClock{}
	tr := &tracedStepper{clk: clk, stepper: &scriptedStepper{clk: clk, steps: []scriptedStep{
		{0, engine.ReasonRoomShort}, {2 * time.Second, engine.ReasonBehindHead}, {3 * time.Second, engine.ReasonBehindHead}}}}
	for range 3 {
		if _, err := tr.step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := tr.take()
	if len(got) != 2 || got["room_short"] != 2 || got["behind_head"] != 3 {
		t.Errorf("seconds %v, want room_short 2 and behind_head 3", got)
	}
	if tr.reason != "behind_head" {
		t.Errorf("last reason %q", tr.reason)
	}
	if g := tr.takeGap(); g != 3*time.Second {
		t.Errorf("longest gap %v, want 3s", g)
	}
	if again := tr.take(); len(again) != 0 {
		t.Errorf("take after take: %v", again)
	}
	if g := tr.takeGap(); g != 0 {
		t.Errorf("gap after takeGap: %v", g)
	}
}

// A wait under 30 s writes only the line at its start, so its admission carries where the time
// went. Mutation: admitted without take() has no seconds.
func TestShortWaitHasReasonSeconds(t *testing.T) {
	b := newBed(t, 128)
	release := holdUntil(t, b.dir, 128)
	timer := time.AfterFunc(700*time.Millisecond, release)
	defer timer.Stop()
	b.admit(t)
	evs := events(t, b.dir)
	if eventNames(evs) != "enqueue waiting admitted" {
		t.Fatalf("events %q", eventNames(evs))
	}
	m := evs[2]
	secs, _ := m["reason_s"].(map[string]any)
	if v, _ := secs["room_short"].(float64); v <= 0 {
		t.Errorf("reason_s %v, want seconds of room_short", m["reason_s"])
	}
	if m["last_reason"] != "room_short" {
		t.Errorf("last_reason %v", m["last_reason"])
	}
}

// holders is cut to the sixteen with the most left to grow into, the largest first. Mutations: no
// cut gives 20; sorting by need keeps the wrong ones.
func TestHoldersCappedAtSixteen(t *testing.T) {
	var in []engine.HolderInfo
	for i := range 20 {
		in = append(in, engine.HolderInfo{Name: fmt.Sprintf("h%02d", i), NeedMiB: 2000 - int64(i), FootprintMiB: 2000 - int64(i)*11, SampleAgeMS: 5})
	}
	in[0].FootprintMiB, in[0].SampleAgeMS = 3000, -1 // over its need: nothing left to grow into
	got := holderLines(in)
	if len(got) != 16 {
		t.Fatalf("%d holders, want 16", len(got))
	}
	for i, h := range got {
		if want := fmt.Sprintf("h%02d", 19-i); h["hold"] != want {
			t.Errorf("holder %d is %v, want %s", i, h["hold"], want)
		}
	}
	if got[0]["sample_age_ms"] != int64(5) {
		t.Errorf("age %v", got[0]["sample_age_ms"])
	}
}

// The largest waiting line of a wait that writes one every 30 s still fits a line of the run log.
// Mutation: a cap of 20 holders makes it too long, and the line is lost.
func TestTwentyHoldersLineFits(t *testing.T) {
	dir := stateDir(t, "")
	label := strings.Repeat("l", spec.MaxLabelBytes)
	name := wire.HoldName(1<<62, 1<<30, 999999, label)
	rep := &engine.Report{Mem: engine.MemInfo{Known: true, FreeMiB: spec.MaxNeedMiB, HeadroomMiB: spec.MaxNeedMiB, HeadroomOK: true, ReserveMiB: spec.MaxNeedMiB}}
	rep.Watch.SampledNS = 1
	for range 20 {
		rep.Holds = append(rep.Holds, engine.HolderInfo{Name: name, NeedMiB: spec.MaxNeedMiB, FootprintMiB: spec.MaxNeedMiB - 1,
			ChildExited: true, OwnerExited: true, SampleAgeMS: 24 * 3600 * 1000})
	}
	secs := map[string]float64{}
	for _, why := range []string{"behind_ticket", "section_busy", "room_short", "behind_head", "watch_level", "watch_spacing", "restart_stale", "restart_level", "restart_calm", "restart_margin"} {
		secs[why] = 29.999
	}
	r := &run{o: runOptions{need: 1, label: label}, rec: telemetry.New(dir, "1762600000000000000-2147483647", label), trace: tracedStepper{secs: secs, maxGap: time.Hour}}
	r.why = func() (string, []engine.HolderInfo) { r.snap = rep; return "", rep.Holds }
	r.waitingLine(engine.Waiting{Position: 9, Ahead: name, Evals: 1 << 30, Outcome: engine.Outcome{Reason: engine.ReasonRestartCalm,
		NoPass: "watch_unreadable", W: 1 << 30, WKnown: true, CalmLeftS: 123456.789}}, time.Hour)
	r.rec.Emit("next", nil)
	evs := events(t, dir)
	if eventNames(evs) != "waiting next" || evs[1]["lost"] != 0.0 {
		t.Fatalf("events %q, lost %v: the line did not fit", eventNames(evs), evs[len(evs)-1]["lost"])
	}
	if h, _ := evs[0]["holders"].([]any); len(h) != 16 {
		t.Errorf("%d holders", len(h))
	}
	data, err := os.ReadFile(filepath.Join(dir, store.TelemetryDir, store.EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	if first, _, _ := bytes.Cut(data, []byte("\n")); len(first)+1 > store.MaxRunLine {
		t.Errorf("%d bytes", len(first)+1)
	}
}

// admitted says what admission read, and its waited_s is the run log's value for the attempt.
// Mutation: waited_s from another clock reading differs from the run log's.
func TestAdmittedLine(t *testing.T) {
	dir := recordedDir(t, 512)
	if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--mem", "128M", "--", "true"); r.code != 0 {
		t.Fatalf("exit %d: %q", r.code, r.stderr)
	}
	m := event(t, events(t, dir), "admitted")
	runs := runLog(t, dir)
	if got, want := m["waited_s"], runs[len(runs)-1]["waited_s"]; got != want || got == nil {
		t.Errorf("waited_s %v, the run log's %v", got, want)
	}
	if _, ok := wire.ParseName(fmt.Sprint(m["hold"])); !ok {
		t.Errorf("hold %v", m["hold"])
	}
	for k, want := range map[string]any{"headroom_mib": 512.0, "headroom_ok": true, "reserve_mib": 0.0, "room_mib": 512.0,
		"watch_gate": "no_holds", "last_reason": ""} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	if s, ok := m["reason_s"].(map[string]any); !ok || len(s) != 0 {
		t.Errorf("reason_s %v, want an empty object for a run admitted at once", m["reason_s"])
	}
	if _, ok := m["w_s"]; ok {
		t.Errorf("w_s on a run that computed none: %v", m)
	}
}

// A sleep during a wait is the clocks' to tell: between enqueue and admitted the clock that counts
// sleep runs ahead of the one that does not, while waited_s stays the wait the run log has.
// Mutations: swapping the two clocks in a line; waited_s read again at the admission.
func TestSleepSeenFromClocks(t *testing.T) {
	const s = int64(time.Second)
	reads := [][2]int64{{1000 * s, 1000 * s}, {1100 * s, 1010 * s}}
	replaceClocks(t, func() (int64, int64) {
		r := reads[0]
		reads = reads[1:]
		return r[0], r[1]
	})
	b := newBed(t, 1024)
	b.admit(t)
	evs := events(t, b.dir)
	if eventNames(evs) != "enqueue admitted" {
		t.Fatalf("events %q", eventNames(evs))
	}
	up := evs[1]["up_ns"].(float64) - evs[0]["up_ns"].(float64)
	awake := evs[1]["awake_ns"].(float64) - evs[0]["awake_ns"].(float64)
	if slept := time.Duration(up - awake); slept != 90*time.Second {
		t.Errorf("slept %v, want 90s", slept)
	}
	if evs[1]["waited_s"] != b.r.waited.Seconds() {
		t.Errorf("waited_s %v, want the run's own %v", evs[1]["waited_s"], b.r.waited.Seconds())
	}
}

// A run's events follow what it read at its start, whichever way the switch is turned while it
// waits. Mutation: reading the switch again at the admission turns either row red.
func TestEventsLatchedAtStart(t *testing.T) {
	const off = "[memory]\nreserve = \"0\"\n"
	for _, c := range []struct {
		name, start, then string
		want              string
	}{
		{"on, then off", telemetryOn, off, "enqueue waiting admitted"},
		{"off, then on", off, telemetryOn, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := recordedDir(t, 128)
			writeConfig(t, dir, c.start)
			release := holdUntil(t, dir, 128)
			cmd := cliCmd(t, baseEnv(dir), "run", "--restart=yes", "--mem", "64M", "--", "true")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 30*time.Second, "the ticket", func() bool { return len(entries(t, dir, "queue")) == 1 })
			writeConfig(t, dir, c.then)
			release()
			_ = cmd.Wait()
			if got := exitStatus(cmd.ProcessState); got != 0 {
				t.Fatalf("exit %d", got)
			}
			if got := eventNames(events(t, dir)); got != c.want {
				t.Errorf("events %q, want %q", got, c.want)
			}
		})
	}
}

// With telemetry off the command's environment gains nothing, and an inherited mark does not get
// through; with it on the command has the mark. Mutations: the helper adding it regardless turns
// the off row red; leaving it out of IsContext passes the inherited one on.
func TestOffAddsNoEnv(t *testing.T) {
	args := append([]string{"run", "--restart=yes", "--"}, helper(t, "getenv", engine.EnvTelemetry)...)
	off := budgetDir(t, 512)
	r := cli(t, append(baseEnv(off), engine.EnvTelemetry+"=1"), args...)
	if r.code != 0 || strings.TrimSpace(r.stdout) != "" {
		t.Errorf("off: exit %d stdout %q stderr %q, want no %s", r.code, r.stdout, r.stderr, engine.EnvTelemetry)
	}
	on := recordedDir(t, 512)
	if r := cli(t, baseEnv(on), args...); r.code != 0 || strings.TrimSpace(r.stdout) != "1" {
		t.Errorf("on: exit %d stdout %q stderr %q, want the mark", r.code, r.stdout, r.stderr)
	}
}

// With telemetry off a whole run calls none of the recorder's readers and makes no directory.
// A panic there would be recovered, so the seam counts. Mutation: a recorder made regardless of
// the switch calls Clocks.
func TestOffCallsNothing(t *testing.T) {
	var calls atomic.Int64
	replaceClocks(t, func() (int64, int64) { calls.Add(1); panic("the recorder read the clocks") })
	dir := stateDir(t, "")
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	r := &run{o: runOptions{need: 1, restart: "no", label: "x", onTimeout: "fail", refusalCode: exitRefusal, argv: []string{"true"}},
		io: IO{Stdin: null, Stdout: null, Stderr: null}, loc: location{dir: dir, cfgPath: filepath.Join(dir, "config.toml")}, start: time.Now()}
	if code := r.ordinary(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("Clocks called %d times with telemetry off", n)
	}
	if hasTelemetryDir(dir) {
		t.Error("telemetry/ exists with telemetry off")
	}
	if r.rec != nil {
		t.Error("a recorder exists with telemetry off")
	}
}

// Oversized gives the switch from its one reading. Mutations: a second reading counts 2; a failed
// read reporting the switch as on reddens the last row.
func TestOversizedReadsOnce(t *testing.T) {
	for _, c := range []struct {
		name, cfg string
		fail      bool
		want      bool
	}{
		{"on", telemetryOn, false, true},
		{"off", "", false, false},
		{"a failed read", telemetryOn, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := stateDir(t, c.cfg)
			st, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			reads := 0
			e := engine.New(st, filepath.Join(dir, "config.toml"), func() (config.Machine, error) {
				reads++
				if c.fail {
					return config.Machine{}, errors.New("no memory reading")
				}
				return config.Machine{MemMiB: 1024, HeadroomMiB: 512, HeadroomOK: true}, nil
			}, engine.Clock{})
			if _, _, got := e.Oversized(1); got != c.want {
				t.Errorf("telemetry %v, want %v", got, c.want)
			}
			if reads != 1 {
				t.Errorf("%d readings, want 1", reads)
			}
		})
	}
}
