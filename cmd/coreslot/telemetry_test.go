//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/telemetry"
	"github.com/core-slot/coreslot/internal/watch"
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
	var out []map[string]any
	for _, m := range allLines(t, dir) {
		if m["e"] != "memory" && m["e"] != "containers" { // the machine's lines are not part of a run's order
			out = append(out, m)
		}
	}
	return out
}

// allLines is every line of events.jsonl, the machine's too.
func allLines(t *testing.T, dir string) []map[string]any {
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
	b.r.o.timeout, b.r.o.typicalS, b.r.overMiB = 90*time.Second, 12, 64
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

// A restart's enqueue is attempt n+1, with its peak as its need, what the need was before, and
// oversized judged on that need. Mutations: requeue leaving needSource alone reads flag; a stale
// attempt reads 1; oversized kept from the first attempt reads false.
func TestEnqueueOfARestart(t *testing.T) {
	b := newBed(t, 1024)
	b.r.overMiB = 200
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
	if m["attempt"] != 2.0 || m["need_source"] != "restart_peak" || m["need_mib"] != 300.0 || m["need_before_mib"] != 128.0 || m["oversized"] != true {
		t.Errorf("the restart's enqueue: %v", m)
	}
	if evs[0]["oversized"] != false {
		t.Errorf("the first enqueue, under the bound: oversized %v", evs[0]["oversized"])
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
	gap := tr.maxGap
	got := tr.take()
	if gap != 3*time.Second {
		t.Errorf("longest gap %v, want 3s", gap)
	}
	if len(got) != 2 || got["room_short"] != 2 || got["behind_head"] != 3 {
		t.Errorf("seconds %v, want room_short 2 and behind_head 3", got)
	}
	if tr.reason != "behind_head" {
		t.Errorf("last reason %q", tr.reason)
	}
	if again := tr.take(); len(again) != 0 || tr.maxGap != 0 {
		t.Errorf("take after take: %v, gap %v", again, tr.maxGap)
	}
}

// A wait that ends in its sleep charges that stretch to its last reason, so reason_s adds up to the
// wait. Mutation: recordWaitEnd without charge reads room_short 2.
func TestWaitEndChargesItsLastStretch(t *testing.T) {
	clk := &fakeClock{}
	dir := stateDir(t, "")
	r := &run{rec: telemetry.New(dir, "1-1", "x"), queued: true, trace: tracedStepper{clk: clk, stepper: &scriptedStepper{clk: clk, steps: []scriptedStep{
		{0, engine.ReasonRoomShort}, {2 * time.Second, engine.ReasonRoomShort}}}}}
	for range 2 {
		if _, err := r.trace.step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	clk.now += time.Second
	r.recordWaitEnd("timeout", telemetry.Fields{})
	if s, _ := event(t, events(t, dir), "timeout")["reason_s"].(map[string]any); s["room_short"] != 3.0 {
		t.Errorf("reason_s %v, want room_short 3", s)
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
		{"on, then off", telemetryOn, off, "enqueue waiting admitted ended"},
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

// With telemetry off a run that waits, writes a waiting line and ticks calls none of the recorder's
// readers and makes no directory. A panic there would be recovered, so the seams count. Mutations:
// a recorder made regardless of the switch calls Clocks; MemoryAsync on a nil recorder calls Sweep;
// sampling with the log off calls Parts.
func TestOffCallsNothing(t *testing.T) {
	var calls, listings atomic.Int64
	replaceTicks(t, 1)
	replaceClocks(t, func() (int64, int64) { calls.Add(1); panic("the recorder read the clocks") })
	savedParts, savedSweep, savedConnect, savedList := store.Parts, telemetry.Sweep, telemetry.Connect, listTree
	store.Parts = func() (map[string]int64, int, bool) { calls.Add(1); panic("the sample read the parts") }
	telemetry.Sweep = func(int) ([]memsample.ProcMem, *memsample.ProcMem, bool) { calls.Add(1); panic("the recorder swept") }
	telemetry.Connect = func(string) (int, string) { calls.Add(1); panic("the recorder connected") }
	listTree = func(pid int) (int64, map[int]memsample.TreeTimes, bool) { listings.Add(1); return savedList(pid) }
	t.Cleanup(func() {
		store.Parts, telemetry.Sweep, telemetry.Connect, listTree = savedParts, savedSweep, savedConnect, savedList
	})
	b := newBed(t, 128)
	writeConfig(t, b.dir, "[memory]\nreserve = \"0\"\n")
	b.r.telemetry, b.r.o.argv = false, []string{"sleep", "2.5"}
	timer := time.AfterFunc(700*time.Millisecond, holdUntil(t, b.dir, 128))
	defer timer.Stop()
	g, code, ok := b.wait(t, context.Background())
	if !ok {
		t.Fatalf("not admitted: code %d, stderr %q", code, b.notices(t))
	}
	if code := b.r.admitted(b.e, g); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(b.notices(t), "waiting for") || listings.Load() < 2 {
		t.Fatalf("the run wrote no waiting line or ticked %d times: %q", listings.Load(), b.notices(t))
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("a telemetry reader was called %d times with telemetry off", n)
	}
	if hasTelemetryDir(b.dir) || b.r.rec != nil {
		t.Errorf("telemetry/ %v, recorder %v with telemetry off", hasTelemetryDir(b.dir), b.r.rec)
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

// wantWaitEnd checks what every event that ends a wait carries.
func wantWaitEnd(t *testing.T, m map[string]any, lastReason string) {
	t.Helper()
	if _, ok := m["waited_s"].(float64); !ok {
		t.Errorf("no waited_s: %v", m)
	}
	if _, ok := m["reason_s"].(map[string]any); !ok {
		t.Errorf("no reason_s: %v", m)
	}
	if m["last_reason"] != lastReason {
		t.Errorf("last_reason %v, want %q", m["last_reason"], lastReason)
	}
}

// A signal that comes with the grant already in hand still ends the wait: the grant is given
// back, so the run was never admitted. Mutation: recording only when no grant is in hand.
func TestInterruptedLineWithGrantInHand(t *testing.T) {
	b := newBed(t, 1024)
	req := b.r.o.request()
	tk, err := b.e.EnqueueContext(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	b.r.recordEnqueue(b.e, tk, req)
	res, err := tk.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, code, ok := b.r.waitOver(b.e, res.(engine.Admitted).Grant, nil, syscall.SIGTERM, false); ok || code != 143 {
		t.Fatalf("code %d, ok %v; want 143 and no grant", code, ok)
	}
	evs := events(t, b.dir)
	if got := eventNames(evs); got != "enqueue interrupted" {
		t.Fatalf("events %q", got)
	}
	if evs[1]["signal"] != "SIGTERM" {
		t.Errorf("signal %v", evs[1]["signal"])
	}
	wantWaitEnd(t, evs[1], "")
	if n := liveHolds(t, b.dir); n != 0 {
		t.Errorf("%d live holds after the interrupt", n)
	}
}

// A release that finds the hold replaced writes nothing after it, on a signal too. Mutation:
// waitOver discarding Close's error writes interrupted.
func TestNoInterruptedAfterFailedRelease(t *testing.T) {
	b := newBed(t, 1024)
	g, code, ok := b.wait(t, context.Background())
	if !ok {
		t.Fatalf("not admitted: %d", code)
	}
	hold := filepath.Join(b.dir, "holds", g.HoldName())
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hold, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.r.waitOver(b.e, g, nil, syscall.SIGTERM, false)
	if got := eventNames(events(t, b.dir)); got != "enqueue admitted" || !b.r.rec.Stopped() {
		t.Errorf("events %q, stopped %v", got, b.r.rec.Stopped())
	}
}

// A state error under --strict after an enqueue refuses the run and says so, unless FORMAT is
// what failed. Mutations: leaving the line out reddens the first row, the FORMAT check the second.
func TestRefusedStrictAfterEnqueue(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"a state error", &store.StateError{Reason: "queue is gone", Err: errors.New("gone")}, "refused"},
		{"a malformed FORMAT", &store.FormatError{Err: errors.New("not a number")}, ""},
		{"a foreign FORMAT", &store.FormatError{N: 99}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := stateDir(t, "")
			null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer null.Close()
			r := &run{o: runOptions{strict: true, refusalCode: exitRefusal}, io: IO{Stderr: null}, rec: telemetry.New(dir, "run-1", "x"), queued: true,
				waited: 3 * time.Second, trace: tracedStepper{reason: "room_short", secs: map[string]float64{"room_short": 2}}}
			if code := r.failOpen(nil, c.err); code != exitRefusal {
				t.Fatalf("exit %d", code)
			}
			evs := events(t, dir)
			if got := eventNames(evs); got != c.want {
				t.Fatalf("events %q, want %q", got, c.want)
			}
			if c.want == "" {
				return
			}
			if evs[0]["reason"] != "strict" || evs[0]["waited_s"] != 3.0 {
				t.Errorf("line %v", evs[0])
			}
			if s, _ := evs[0]["reason_s"].(map[string]any); s["room_short"] != 2.0 {
				t.Errorf("reason_s %v", evs[0]["reason_s"])
			}
			wantWaitEnd(t, evs[0], "room_short")
		})
	}
}

// A timeout says what --on-timeout asked. Mutation: on_timeout written as a constant reddens a row.
func TestTimeoutLine(t *testing.T) {
	for _, c := range []struct {
		onTimeout string
		code      int
	}{{"fail", 75}, {"run", 0}} {
		t.Run(c.onTimeout, func(t *testing.T) {
			dir := recordedDir(t, 128)
			holdOther(t, dir, 128)
			r := cli(t, baseEnv(dir), "run", "--restart=yes", "--mem", "64M", "--timeout", "1s", "--on-timeout", c.onTimeout, "--", "true")
			if r.code != c.code {
				t.Fatalf("exit %d: %q", r.code, r.stderr)
			}
			evs := events(t, dir)
			tail := " timeout" // a run outside the budget says so and ends too
			if c.onTimeout == "run" {
				tail = " timeout outside ended"
			}
			if got := eventNames(evs); !strings.HasPrefix(got, "enqueue waiting") || !strings.HasSuffix(got, tail) {
				t.Fatalf("events %q", got)
			}
			m := event(t, evs, "timeout")
			if m["on_timeout"] != c.onTimeout {
				t.Errorf("on_timeout %v", m["on_timeout"])
			}
			wantWaitEnd(t, m, "room_short")
			if s, _ := m["reason_s"].(map[string]any); s["room_short"] == nil {
				t.Errorf("reason_s %v, want seconds of room_short", m["reason_s"])
			}
		})
	}
}

// A restart whose enqueue fails has no wait to report. Mutation: dropping the queued check in
// recordWaitEnd writes a timeout for an attempt that never queued.
func TestRestartEnqueueFailureRecordsNothing(t *testing.T) {
	b := newBed(t, 1024)
	req := b.r.o.request()
	first, _, ok := b.r.wait(context.Background(), func() {}, b.e, req)
	if !ok {
		t.Fatalf("not admitted: %q", b.notices(t))
	}
	b.r.stops = 1
	b.r.requeue(&req, first)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, _, ok := b.r.wait(ctx, cancel, b.e, req); ok {
		t.Fatal("admitted")
	}
	if got := eventNames(events(t, b.dir)); got != "enqueue admitted" {
		t.Errorf("events %q, want the first attempt's alone", got)
	}
}

// ended is an in-process run whose command ends, over a state directory with its layout and
// telemetry on.
type ended struct {
	dir string
	st  *store.Store
	r   *run
}

func newEnded(t *testing.T, argv ...string) *ended {
	t.Helper()
	dir := recordedDir(t, 512)
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { null.Close() })
	r := &run{o: runOptions{need: 1, restart: "no", noRestart: true, label: "go test", onTimeout: "fail", refusalCode: exitRefusal, argv: argv},
		io: IO{Stdin: null, Stdout: null, Stderr: null}, loc: location{dir: dir, cfgPath: filepath.Join(dir, "config.toml")},
		start: time.Now(), telemetry: true}
	r.rec = telemetry.New(dir, "1-1", "go test")
	return &ended{dir: dir, st: st, r: r}
}

func (b *ended) child(failOpen, code string) child {
	return child{env: os.Environ(), release: func() error { return nil }, log: b.st, failOpen: failOpen, failCode: code}
}

// ended is the run log's line, joined: its record is the appended line with fail_open emptied, and
// its time_ns is exact. Mutations: a time_ns taken again at the end; the record built apart from
// the one appended.
func TestEndedJoinsTheRunLog(t *testing.T) {
	b := newEnded(t, "true")
	if code := b.r.runChild(b.child("", "")); code != 0 {
		t.Fatalf("exit %d", code)
	}
	data, err := os.ReadFile(filepath.Join(b.dir, store.TelemetryDir, store.EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	var line struct {
		E      string          `json:"e"`
		Record store.RunRecord `json:"record"`
	}
	if err := json.Unmarshal(bytes.Split(bytes.TrimSpace(data), []byte("\n"))[0], &line); err != nil {
		t.Fatal(err)
	}
	runs, _, err := b.st.ReadRuns()
	if err != nil || len(runs) != 2 { // the setup run's line and this one
		t.Fatalf("run log %v, %d lines", err, len(runs))
	}
	if line.E != "ended" || !reflect.DeepEqual(line.Record, runs[1]) || line.Record.TimeNS != b.r.start.UnixNano() {
		t.Errorf("ended %+v, the run log's %+v, want start %d", line, runs[1], b.r.start.UnixNano())
	}
}

// A death by SIGKILL is a signal and an outsider's; a command's own exit 137 is neither.
// Mutation: reading exit == 137 for the signal.
func TestEndedSignalNotExit137(t *testing.T) {
	b := newEnded(t, "sh", "-c", "exit 137")
	if code := b.r.runChild(b.child("", "")); code != 137 {
		t.Fatalf("exit %d", code)
	}
	m := event(t, events(t, b.dir), "ended")
	if _, ok := m["signal"]; ok {
		t.Errorf("signal on an exit 137: %v", m["signal"])
	}
	if _, ok := m["stopped_by"]; ok {
		t.Errorf("stopped_by on an exit 137: %v", m["stopped_by"])
	}
	k := newEnded(t, "sh", "-c", "kill -9 $$")
	k.r.runChild(k.child("", ""))
	m = event(t, events(t, k.dir), "ended")
	if m["signal"] != "SIGKILL" || m["stopped_by"] != "outside" {
		t.Errorf("signal %v stopped_by %v, want SIGKILL and outside", m["signal"], m["stopped_by"])
	}
}

// The mean is the integer MiB of the sampled sum over the readings that succeeded.
// Mutations: dividing by the peak's reading or by the ticks; keeping the rounding of a float.
func TestMeanFootprint(t *testing.T) {
	var n atomic.Int64
	saved, savedClient := listTree, listClient
	listTree = func(int) (int64, map[int]memsample.TreeTimes, bool) {
		switch n.Add(1) {
		case 1:
			return 100 << 10, nil, true
		case 2:
			return 301 << 10, nil, true
		}
		return 0, nil, false
	}
	listClient = func(int) bool { return false }
	t.Cleanup(func() { listTree, listClient = saved, savedClient })
	b := newEnded(t, "sleep", "2.6")
	b.r.runChild(b.child("", ""))
	if got := event(t, events(t, b.dir), "ended")["mean_footprint_mib"]; got != 200.0 {
		t.Errorf("mean_footprint_mib %v, want 200 (the integer of 200.5)", got)
	}
}

// A run under a second has no sample: no mean, and the exit status and the run-log line stand.
// Mutation: the mean taken as sum/samples without the guard divides by zero and loses the line.
func TestSubSecondRunHasNoMean(t *testing.T) {
	b := newEnded(t, "true")
	if code := b.r.runChild(b.child("", "")); code != 0 {
		t.Fatalf("exit %d", code)
	}
	m := event(t, events(t, b.dir), "ended")
	if _, ok := m["mean_footprint_mib"]; ok {
		t.Errorf("a mean with no sample: %v", m["mean_footprint_mib"])
	}
	if runs := runLog(t, b.dir); len(runs) != 2 {
		t.Errorf("%d run-log lines, want the setup's and this run's", len(runs))
	}
}

// A signal between the admission and the start names itself; a release that found its files
// replaced silences the line. Mutation: the release error dropped, as before.
func TestNotStartedSignal(t *testing.T) {
	for _, c := range []struct {
		name    string
		release error
		want    string
	}{
		{"released", nil, "not_started"},
		{"identity mismatch", &engine.IdentityError{Err: &store.StateError{Reason: "x"}}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newEnded(t, "true")
			runSignals <- syscall.SIGTERM
			ch := b.child("", "")
			ch.release = func() error { return c.release }
			b.r.runChild(ch)
			evs := events(t, b.dir)
			if eventNames(evs) != c.want {
				t.Fatalf("events %q, want %q", eventNames(evs), c.want)
			}
			if c.want != "" && evs[0]["reason"] != "SIGTERM" {
				t.Errorf("reason %v", evs[0]["reason"])
			}
		})
	}
}

// After a release identity mismatch the recorder writes nothing more. Mutation: dropping Stop.
func TestReleaseFailedSilencesRecorder(t *testing.T) {
	b := newEnded(t, "true")
	b.r.rec.Emit("probe", nil)
	if !b.r.releaseFailed(&engine.IdentityError{Err: &store.StateError{Reason: "x"}}) {
		t.Fatal("a mismatch did not stop the run")
	}
	b.r.rec.Emit("after", nil)
	if got := eventNames(events(t, b.dir)); got != "probe" {
		t.Errorf("events %q, want only probe", got)
	}
}

// A stop names the round, the mode, the level, the hold's place in the list and the excess; in
// observe mode it is written too and nothing is killed. Mutations: round from the level; the
// position of another hold; a line written before the kill succeeded.
func TestStoppedLine(t *testing.T) {
	for _, c := range []struct {
		name         string
		floor, obs   bool
		round, level string
	}{{"red", false, false, "red", "red"}, {"floor", true, false, "floor", "floor"}, {"observe", false, true, "red", "red"}} {
		t.Run(c.name, func(t *testing.T) {
			b := newEnded(t, "true")
			b.r.hold = "b"
			lvl := watch.Red
			if c.floor {
				lvl = watch.Floor
			}
			det := watch.Detector{Level: lvl, Excess: 12.5, RoundFloor: c.floor, Observed: c.obs, Stop: []string{"a", "b"}}
			killed := false
			b.r.stopFor(det, 512<<10, memsample.TreeTimes{}, false, func() bool { killed = true; return true })
			if killed == c.obs {
				t.Errorf("killed %v in a run with observe %v", killed, c.obs)
			}
			m := event(t, events(t, b.dir), "stopped")
			for k, want := range map[string]any{"round": c.round, "observe": c.obs, "level": c.level, "list_pos": 1.0, "excess": 12.5} {
				if m[k] != want {
					t.Errorf("%s = %v, want %v", k, m[k], want)
				}
			}
		})
	}
	b := newEnded(t, "true")
	b.r.hold = "b"
	b.r.stopFor(watch.Detector{Stop: []string{"b"}}, 0, memsample.TreeTimes{}, false, func() bool { return false })
	if got := eventNames(events(t, b.dir)); got != "" {
		t.Errorf("a stop that killed nothing wrote %q", got)
	}
}

// A keep is written once for its round. Mutation: no guard on the round already said.
func TestKeptLine(t *testing.T) {
	b := newEnded(t, "true")
	det := watch.Detector{Level: watch.Red, Excess: 7, LastRoundNS: 5e9, Stop: []string{"a"},
		Kept: map[string]watch.Kept{"b": {RoundNS: 5e9, DeadlineNS: 9e9, LeftS: 4, Done: 0.8}}}
	last := b.r.keptNotice(det, "b", 0)
	if last != 5e9 {
		t.Fatalf("round %d", last)
	}
	b.r.keptNotice(det, "b", last)
	evs := events(t, b.dir)
	if eventNames(evs) != "kept" {
		t.Fatalf("events %q, want one kept", eventNames(evs))
	}
	for k, want := range map[string]any{"round": "red", "observe": false, "level": "red", "excess": 7.0} {
		if evs[0][k] != want {
			t.Errorf("%s = %v, want %v", k, evs[0][k], want)
		}
	}
	if _, ok := evs[0]["list_pos"]; ok {
		t.Errorf("a kept hold is not in the list: %v", evs[0]["list_pos"])
	}
}

// setFormat replaces the state directory's FORMAT.
func setFormat(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, store.FormatFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// labelled is the lines of the run labelled label.
func labelled(evs []map[string]any, label string) []map[string]any {
	var out []map[string]any
	for _, m := range evs {
		if m["label"] == label {
			out = append(out, m)
		}
	}
	return out
}

// replaceTicks makes every n listings a live line; the test's end restores it.
func replaceTicks(t *testing.T, n int) {
	t.Helper()
	saved := telemetryTicks
	telemetryTicks = n
	t.Cleanup(func() { telemetryTicks = saved })
}

// A pass-through inside its parent, and one under an unbudgeted parent, record an outside line
// and an ended line, with a closed reason. Mutations: the parent's free-text reason on the line;
// a pass-through that records nothing; ended left out.
func TestOutsidePassThrough(t *testing.T) {
	t.Run("inside the parent", func(t *testing.T) {
		dir := recordedDir(t, 1024)
		r := outer(t, dir, "256M", `"$BIN" run --label inner --mem 128M -- true`)
		if r.code != 0 {
			t.Fatalf("exit %d: %q", r.code, r.stderr)
		}
		evs := events(t, dir)
		inner := labelled(evs, "inner")
		if got := eventNames(inner); got != "outside ended" {
			t.Fatalf("inner events %q among %q", got, eventNames(evs))
		}
		o := inner[0]
		if o["kind"] != "pass_through" || o["reason"] != "parent_hold" || o["need_mib"] != 128.0 {
			t.Errorf("outside %v", o)
		}
		if want := event(t, evs, "admitted")["hold"]; o["parent"] != want {
			t.Errorf("parent %v, want the outer hold %v", o["parent"], want)
		}
		if _, ok := inner[1]["hold"]; ok {
			t.Errorf("ended of a pass-through has a hold: %v", inner[1])
		}
	})
	t.Run("under an unbudgeted parent", func(t *testing.T) {
		dir := recordedDir(t, 1024)
		env := append(baseEnv(dir), engine.EnvUnbudgeted+"=/Users/ana/secret", engine.EnvTelemetry+"=1")
		if r := cli(t, env, "run", "--restart=yes", "--label", "inner", "--", "true"); r.code != 0 {
			t.Fatalf("exit %d: %q", r.code, r.stderr)
		}
		evs := events(t, dir)
		if got := eventNames(evs); got != "outside ended" {
			t.Fatalf("events %q", got)
		}
		if evs[0]["reason"] != "parent_unbudgeted" || evs[1]["fail_open"] != "parent_unbudgeted" {
			t.Errorf("lines %v", evs)
		}
		data, _ := os.ReadFile(filepath.Join(dir, store.TelemetryDir, store.EventsFile))
		if strings.Contains(string(data), "secret") {
			t.Errorf("the parent's reason reached the line: %s", data)
		}
	})
}

// With no mark in its environment a pass-through opens nothing; with one it opens the directory
// once. Mutation: the mark checked after the read makes the count 1 with telemetry off.
func TestPassThroughOffReadsNothing(t *testing.T) {
	b := newEnded(t, "true")
	b.r.rec = nil
	opens := 0
	saved := openReadOnly
	openReadOnly = func(dir string) (*store.Store, error) { opens++; return saved(dir) }
	t.Cleanup(func() { openReadOnly = saved })
	if code := b.r.passThrough(map[string]string{}, "parent"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if opens != 0 || b.r.rec != nil || hasTelemetryDir(b.dir) {
		t.Errorf("off: %d opens, recorder %v, telemetry/ %v", opens, b.r.rec, hasTelemetryDir(b.dir))
	}
	if code := b.r.passThrough(map[string]string{engine.EnvTelemetry: "1"}, "parent"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if opens != 1 {
		t.Errorf("on: %d opens, want 1", opens)
	}
}

// A pass-through into a FORMAT it cannot read writes nothing and hands its command no mark, and
// one whose command changes FORMAT writes no last line. Mutations: the FORMAT read left out; the
// re-read before the last line left out; the inherited mark passed on.
func TestPassThroughRefusesForeignFormat(t *testing.T) {
	env := func(dir string) []string {
		return append(baseEnv(dir), engine.EnvUnbudgeted+"=parent", engine.EnvTelemetry+"=1")
	}
	for name, body := range map[string]string{"foreign": "coreslot-protocol 3\n", "malformed": "coreslot-protocol x\n"} {
		t.Run(name, func(t *testing.T) {
			dir := recordedDir(t, 1024)
			setFormat(t, dir, body)
			r := cli(t, env(dir), append([]string{"run", "--restart=yes", "--"}, helper(t, "getenv", engine.EnvTelemetry)...)...)
			if r.code != 0 || strings.TrimSpace(r.stdout) != "" {
				t.Errorf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
			}
			if hasTelemetryDir(dir) {
				t.Errorf("events %q", eventNames(events(t, dir)))
			}
		})
	}
	t.Run("changed while it ran", func(t *testing.T) {
		dir := recordedDir(t, 1024)
		e := append(env(dir), "S="+dir)
		if r := cli(t, e, "run", "--restart=yes", "--", "sh", "-c", `printf 'coreslot-protocol 3\n' > "$S/FORMAT"`); r.code != 0 {
			t.Fatalf("exit %d: %q", r.code, r.stderr)
		}
		if got := eventNames(events(t, dir)); got != "outside" {
			t.Errorf("events %q, want the outside line alone", got)
		}
	})
}

// A state directory that cannot be located gives a pass-through no recorder, so nothing appears
// in the working directory. Mutation: the location's error not checked reddens both rows.
func TestPassThroughBadLocationWritesNothing(t *testing.T) {
	t.Run("relative directory", func(t *testing.T) {
		cwd := t.TempDir()
		cmd := exec.Command(cliBinary(t), "run", "--restart=yes", "--", "true")
		cmd.Dir = cwd
		for _, kv := range baseEnv(cwd) {
			if !strings.HasPrefix(kv, "CORESLOT_STATE_DIR=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(cmd.Env, "CORESLOT_STATE_DIR=state", engine.EnvUnbudgeted+"=parent", engine.EnvTelemetry+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if names := entries(t, cwd, ""); len(names) != 0 {
			t.Errorf("the working directory got %q", names)
		}
	})
	t.Run("in process", func(t *testing.T) {
		b := newEnded(t, "true")
		b.r.rec, b.r.loc.err = nil, errors.New("no home")
		if code := b.r.passThrough(map[string]string{engine.EnvTelemetry: "1"}, "parent"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if hasTelemetryDir(b.dir) {
			t.Errorf("events %q", eventNames(events(t, b.dir)))
		}
	})
}

// A state error with its unbudgeted hold published makes the recorder, if it has none yet, and
// says why the run is outside the budget. Mutation: a recorder only after an enqueue loses both
// lines.
func TestOutsideFailOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	dir := recordedDir(t, 1024)
	queue := filepath.Join(dir, "queue")
	if err := os.Chmod(queue, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(queue, 0o700) })
	r := cli(t, baseEnv(dir), "run", "--restart=yes", "--mem", "64M", "--", "true")
	if r.code != 0 || !strings.Contains(r.stderr, "fail-open") {
		t.Fatalf("exit %d: %q", r.code, r.stderr)
	}
	evs := events(t, dir)
	if got := eventNames(evs); got != "outside ended" {
		t.Fatalf("events %q", got)
	}
	if evs[0]["kind"] != "fail_open" || evs[0]["reason"] != "state_error" || evs[0]["need_mib"] != 64.0 || evs[1]["fail_open"] != "state_error" {
		t.Errorf("lines %v", evs)
	}
	if strings.Contains(fmt.Sprint(evs), dir) {
		t.Errorf("a path on a line: %v", evs)
	}
}

// A restart that fails open is its own attempt, and its ended names the hold it published.
// Mutations: no SetAttempt before outside reads attempt 1; hold taken from the grant is absent.
func TestRestartFailOpenAttemptAndHold(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	b := newBed(t, 1024)
	b.r.env = os.Environ()
	req := b.r.o.request()
	first, _, ok := b.r.wait(context.Background(), func() {}, b.e, req)
	if !ok {
		t.Fatalf("not admitted: %q", b.notices(t))
	}
	b.r.stops = 1
	b.r.requeue(&req, first)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	queue := filepath.Join(b.dir, "queue")
	if err := os.Chmod(queue, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(queue, 0o700) })
	if _, code, ok := b.r.wait(context.Background(), func() {}, b.e, req); ok || code != 0 {
		t.Fatalf("ok %v, exit %d: %q", ok, code, b.notices(t))
	}
	evs := events(t, b.dir)
	if got := eventNames(evs); got != "enqueue admitted outside ended" {
		t.Fatalf("events %q", got)
	}
	if evs[2]["attempt"] != 2.0 || evs[3]["attempt"] != 2.0 {
		t.Errorf("attempts %v, %v, want 2", evs[2]["attempt"], evs[3]["attempt"])
	}
	if _, ok := wire.ParseName(fmt.Sprint(evs[3]["hold"])); !ok {
		t.Errorf("ended's hold %v", evs[3]["hold"])
	}
}

// failOpenBed is a bed whose run has already queued: a live recorder and one line written.
func failOpenBed(t *testing.T) *bed {
	t.Helper()
	b := newBed(t, 1024)
	b.r.rec = telemetry.New(b.dir, "1-1", "go test")
	b.r.rec.Emit("probe", nil)
	b.r.o.argv = []string{"sh", "-c", `test -z "$` + engine.EnvTelemetry + `"`} // exit 1 with the mark
	return b
}

// Each of these has a command that fails if it was given the mark.
func wantSilent(t *testing.T, b *bed, code, want int) {
	t.Helper()
	if code != want {
		t.Errorf("exit %d, want %d (stderr %q)", code, want, b.notices(t))
	}
	if got := eventNames(events(t, b.dir)); got != "probe" {
		t.Errorf("events %q, want the probe alone", got)
	}
}

// A foreign FORMAT ends the recording, under --strict too, and the command gets no mark.
// Mutation: dropping the Stop at the head of failOpen.
func TestFailOpenForeignFormatRecordsNothing(t *testing.T) {
	t.Run("first sight", func(t *testing.T) {
		dir := recordedDir(t, 1024)
		setFormat(t, dir, "coreslot-protocol 3\n")
		r := cli(t, baseEnv(dir), append([]string{"run", "--restart=yes", "--"}, helper(t, "getenv", engine.EnvTelemetry)...)...)
		if r.code != 0 || strings.TrimSpace(r.stdout) != "" || hasTelemetryDir(dir) {
			t.Errorf("exit %d stdout %q telemetry/ %v", r.code, r.stdout, hasTelemetryDir(dir))
		}
	})
	t.Run("after an enqueue", func(t *testing.T) {
		b := failOpenBed(t)
		setFormat(t, b.dir, "coreslot-protocol 3\n")
		wantSilent(t, b, b.r.failOpen(b.e, &store.FormatError{N: 3}), 0)
	})
}

// A malformed FORMAT reaches failOpen as a plain state error from a step: no hold can be
// published, and --strict has nothing to record. Mutations: the else branch's Stop; the FORMAT
// re-read at the head of failOpen reddens "strict".
func TestFailOpenMalformedFormatRecordsNothing(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{false: "fail-open", true: "strict"}[strict], func(t *testing.T) {
			b := failOpenBed(t)
			b.r.o.strict, b.r.queued = strict, true
			setFormat(t, b.dir, "coreslot-protocol x\n")
			want := 0
			if strict {
				want = exitRefusal
			}
			wantSilent(t, b, b.r.failOpen(b.e, &store.StateError{Reason: `malformed FORMAT "coreslot-protocol x"`}), want)
		})
	}
}

// With no state directory no recorder survives. Mutation: dropping the Stop lets ended through.
func TestFailOpenNilEngineRecordsNothing(t *testing.T) {
	b := failOpenBed(t)
	wantSilent(t, b, b.r.failOpen(nil, &store.StateError{Reason: "no directory"}), 0)
}

// A run that timed out and runs outside the budget says so and reports its footprint while it
// runs. Mutations: the outside line left out; the live line needs a grant; a listing's KiB
// written as MiB.
func TestOutsideTimeoutRunThenLive(t *testing.T) {
	replaceTicks(t, 1)
	b := newBed(t, 128)
	holdUntil(t, b.dir, 128)
	b.r.o.need, b.r.o.onTimeout, b.r.o.timeout = 64, "run", 300*time.Millisecond
	b.r.o.argv = []string{"sleep", "2.5"}
	saved := listTree
	listTree = func(int) (int64, map[int]memsample.TreeTimes, bool) { return 5 << 10, nil, true }
	t.Cleanup(func() { listTree = saved })
	ctx, cancel := context.WithTimeout(context.Background(), b.r.o.timeout)
	defer cancel()
	if _, code, ok := b.r.wait(ctx, cancel, b.e, b.r.o.request()); ok || code != 0 {
		t.Fatalf("ok %v, exit %d: %q", ok, code, b.notices(t))
	}
	evs := events(t, b.dir)
	got := eventNames(evs)
	if !strings.HasPrefix(got, "enqueue waiting") || !strings.Contains(got, " timeout outside live") || !strings.HasSuffix(got, " live ended") {
		t.Fatalf("events %q", got)
	}
	o := event(t, evs, "outside")
	if o["kind"] != "timeout_run" || o["reason"] != "timeout" || o["need_mib"] != 64.0 {
		t.Errorf("outside %v", o)
	}
	if got := event(t, evs, "live")["footprint_mib"]; got != 5.0 {
		t.Errorf("footprint_mib %v, want 5", got)
	}
}

// A section that stays busy past the wait leaves the timeout run with no hold and, without an
// enqueue, no recorder: nothing read FORMAT in a section. Mutation: making the recorder whenever
// the run log is kept.
func TestTimeoutRunWithoutEnqueueRecordsNothing(t *testing.T) {
	b := newBed(t, 1024)
	b.r.o.argv = []string{"true"}
	sec, err := b.e.Store().Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer sec.Leave()
	if code := b.r.timeoutRun(b.e); code != 0 {
		t.Fatalf("exit %d: %q", code, b.notices(t))
	}
	if !strings.Contains(b.notices(t), "the unbudgeted hold was not published") {
		t.Errorf("stderr %q: the section was not busy", b.notices(t))
	}
	if hasTelemetryDir(b.dir) {
		t.Errorf("events %q", eventNames(events(t, b.dir)))
	}
}

// A timeout run that finds a FORMAT it cannot read writes nothing more and hands its command no
// mark. Mutation: dropping the Stop in timeoutRun lets ended through.
func TestTimeoutRunForeignFormatRecordsNothing(t *testing.T) {
	b := failOpenBed(t)
	setFormat(t, b.dir, "coreslot-protocol 3\n")
	wantSilent(t, b, b.r.timeoutRun(b.e), 0)
}

// fakeSweep makes the process sweep a fixed one that counts its calls; the test's end restores it.
func fakeSweep(t *testing.T) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	saved := telemetry.Sweep
	telemetry.Sweep = func(int) ([]memsample.ProcMem, *memsample.ProcMem, bool) {
		calls.Add(1)
		return []memsample.ProcMem{{Name: "Safari", FootprintMiB: 900}}, nil, true
	}
	t.Cleanup(func() { telemetry.Sweep = saved })
	return &calls
}

// memoryLines is the memory lines written so far.
func memoryLines(t *testing.T, dir string) (out []map[string]any) {
	for _, m := range allLines(t, dir) {
		if m["e"] == "memory" {
			out = append(out, m)
		}
	}
	return out
}

// A waiting run and a run outside the budget each write a memory line, which names no run, and
// within one 30 s window only one. Mutations: the call left out of recordWaiting or of watchChild
// turns its row red; the window not claimed writes one per tick.
func TestMemoryLineFromWaitingAndRunning(t *testing.T) {
	t.Run("waiting", func(t *testing.T) {
		calls := fakeSweep(t)
		b := newBed(t, 128)
		holdUntil(t, b.dir, 128)
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		b.wait(t, ctx)
		waitFor(t, 5*time.Second, "the memory line", func() bool { return len(memoryLines(t, b.dir)) == 1 })
		if l := memoryLines(t, b.dir)[0]; l["run"] != nil || calls.Load() != 1 {
			t.Errorf("line %v after %d sweeps", l, calls.Load())
		}
	})
	t.Run("running outside the budget", func(t *testing.T) {
		replaceTicks(t, 1)
		calls := fakeSweep(t)
		b := newEnded(t, "sleep", "2.5")
		if code := b.r.runChild(b.child("", "")); code != 0 {
			t.Fatalf("exit %d", code)
		}
		waitFor(t, 5*time.Second, "the memory line", func() bool { return len(memoryLines(t, b.dir)) >= 1 })
		time.Sleep(200 * time.Millisecond)
		if n := len(memoryLines(t, b.dir)); n != 1 || calls.Load() != 1 {
			t.Errorf("%d memory lines and %d sweeps in one window, want 1 and 1", n, calls.Load())
		}
	})
}

// The memory of a pass-through is its parent's: no live lines, and no memory lines from it.
// Mutations: live for any run with no grant; the memory call outside the log test.
func TestNoLiveForPassThrough(t *testing.T) {
	replaceTicks(t, 1)
	calls := fakeSweep(t)
	b := newEnded(t, "sleep", "1.5")
	b.r.rec = nil
	if code := b.r.passThrough(map[string]string{engine.EnvTelemetry: "1"}, "parent"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := eventNames(events(t, b.dir)); got != "outside ended" {
		t.Errorf("events %q", got)
	}
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 || len(memoryLines(t, b.dir)) != 0 {
		t.Errorf("a pass-through swept %d times and wrote %d memory lines", calls.Load(), len(memoryLines(t, b.dir)))
	}
}

// fakeEngine is a local Docker socket named by DOCKER_HOST with no containers, and the count of
// connections it took; every connection through the seam is counted too, so one that a bug sends
// elsewhere still shows.
func fakeEngine(t *testing.T) (conns, dials *atomic.Int64) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	conns, dials = new(atomic.Int64), new(atomic.Int64)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			io.ReadAtLeast(c, make([]byte, 16), 16)
			io.WriteString(c, "HTTP/1.0 200 OK\r\n\r\n[]")
			c.Close()
		}
	}()
	saved := telemetry.Connect
	telemetry.Connect = func(p string) (int, string) { dials.Add(1); return saved(p) }
	t.Cleanup(func() { telemetry.Connect = saved })
	t.Setenv("DOCKER_HOST", "unix://"+path)
	return conns, dials
}

// containerLines is the containers lines written so far.
func containerLines(t *testing.T, dir string) (out []map[string]any) {
	for _, m := range allLines(t, dir) {
		if m["e"] == "containers" {
			out = append(out, m)
		}
	}
	return out
}

// A waiter never connects, not even a nested one whose own command is container work while another
// hold is: only supervision does. Mutation: ContainersAsync called from recordWaiting.
func TestWaiterNeverConnects(t *testing.T) {
	conns, dials := fakeEngine(t)
	fakeSweep(t)
	b := newBed(t, 128)
	holdUntil(t, b.dir, 128)
	hold := filepath.Join(b.dir, "holds", entries(t, b.dir, "holds")[0])
	body, err := os.ReadFile(hold)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hold, bytes.Replace(body, []byte("{"), []byte(`{"container":true,`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	b.r.parentHold, b.r.o.argv, b.r.o.pinned = filepath.Base(hold), []string{"docker", "build", "."}, true
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	b.wait(t, ctx)
	waitFor(t, 5*time.Second, "the waiter's memory line", func() bool { return len(memoryLines(t, b.dir)) == 1 })
	time.Sleep(100 * time.Millisecond)
	if conns.Load() != 0 || dials.Load() != 0 || len(containerLines(t, b.dir)) != 0 {
		t.Errorf("%d connections, %d dials, %d lines from a waiter", conns.Load(), dials.Load(), len(containerLines(t, b.dir)))
	}
}

// Only a supervised run that is container work, with telemetry on, reads the socket: not one that
// is not, not one with telemetry off, not a pass-through. Mutations: the call without the p.container
// test reddens the first row; the call outside the log test reddens the pass-through; a read that
// does not go through the recorder, or MemoryAsync on a nil recorder calling Sweep, reddens the off
// row; leaving the call out reddens the last.
func TestNoContainerRunNeverConnects(t *testing.T) {
	for _, c := range []struct {
		name               string
		client, off, inner bool
		want               int64
	}{
		{name: "no container work"},
		{name: "container work, telemetry off", client: true, off: true},
		{name: "container work in a pass-through", client: true, inner: true},
		{name: "container work", client: true, want: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			replaceTicks(t, 1)
			sweeps := fakeSweep(t)
			conns, dials := fakeEngine(t)
			saved := listClient
			listClient = func(int) bool { return c.client }
			t.Cleanup(func() { listClient = saved })
			argv := []string{"sleep", "1.5"}
			if c.inner { // listClient is not asked without a log: the command itself must be a client
				docker := filepath.Join(t.TempDir(), "docker")
				if err := os.WriteFile(docker, []byte("#!/bin/sh\nsleep 1.5\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				argv = []string{docker}
			}
			b := newEnded(t, argv...)
			if c.off {
				b.r.rec = nil
			}
			var code int
			if c.inner {
				b.r.rec = nil
				code = b.r.passThrough(map[string]string{engine.EnvTelemetry: "1"}, "parent")
			} else {
				code = b.r.runChild(b.child("", ""))
			}
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			if c.want > 0 {
				waitFor(t, 5*time.Second, "the containers line", func() bool { return len(containerLines(t, b.dir)) == 1 })
				if l := containerLines(t, b.dir)[0]; l["run"] != nil || l["containers_total"] != 0.0 {
					t.Errorf("line %v", l)
				}
			}
			time.Sleep(100 * time.Millisecond)
			if conns.Load() != c.want || dials.Load() != c.want {
				t.Errorf("%d connections, %d dials, want %d", conns.Load(), dials.Load(), c.want)
			}
			if c.off && sweeps.Load() != 0 {
				t.Errorf("%d sweeps with telemetry off", sweeps.Load())
			}
		})
	}
}

// A process inside another run's command never connects, even --independent with container work
// of its own: its socket would read as the outer command's. Mutations: the inherited test left out
// of watchChild reddens both rows; either half of it left out of decide reddens its row.
func TestNestedIndependentNeverConnects(t *testing.T) {
	conns, _ := fakeEngine(t)
	bin := t.TempDir()
	docker := filepath.Join(bin, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nsleep 1.5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	dir := recordedDir(t, 1024)
	for _, c := range []struct {
		name string
		run  func() result
	}{
		{"inherited hold", func() result {
			return outer(t, dir, "256M", `"$BIN" run --independent --label inner --mem 128M -- "$DOCKER"`, ticksEnv+"=1", "DOCKER="+docker)
		}},
		{"inherited unbudgeted", func() result {
			return cli(t, append(baseEnv(dir), ticksEnv+"=1", engine.EnvUnbudgeted+"=parent"), "run", "--restart=yes", "--independent", "--label", "inner", "--mem", "128M", "--", docker)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := len(labelled(events(t, dir), "inner"))
			if r := c.run(); r.code != 0 {
				t.Fatalf("exit %d: %q", r.code, r.stderr)
			}
			if got := labelled(events(t, dir), "inner"); len(got) == before || got[len(got)-1]["e"] != "ended" {
				t.Fatalf("the nested run did not record: %q", eventNames(got))
			}
			if conns.Load() != 0 || len(containerLines(t, dir)) != 0 {
				t.Errorf("%d connections, %d lines from a nested run", conns.Load(), len(containerLines(t, dir)))
			}
		})
	}
}

// DOCKER_HOST with a unix socket is the only candidate; one that is not a unix socket is a code; with
// neither, the home's two sockets come before the system's, and with no home located only the
// system's is tried. Mutations: a unix host still searching the defaults; a tcp host falling through
// to them; the home's sockets after the system's.
func TestDockerSockets(t *testing.T) {
	sys := "/var/run/docker.sock"
	run, desktop := "/Users/ana/.docker/run/docker.sock", "/Users/ana/.docker/desktop/docker.sock"
	for _, c := range []struct {
		name, host, home string
		want             []string
	}{
		{"unix host", "unix:///tmp/x/docker.sock", "/Users/ana", []string{"/tmp/x/docker.sock"}},
		{"tcp host", "tcp://10.0.0.1:2375", "/Users/ana", []string{telemetry.TCPHost}},
		{"ssh host", "ssh://ana@server", "/Users/ana", []string{telemetry.TCPHost}},
		{"defaults", "", "/Users/ana", []string{run, desktop, sys}},
		{"no home located", "", "", []string{sys}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := dockerSockets(c.host, c.home); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// The location carries the home it looked up, and under the state-directory override it looks up
// none. Mutations: home left unset reddens the first check; a lookup under the override the second.
func TestLocationKnowsTheHome(t *testing.T) {
	t.Setenv(config.StateDirEnv, "")
	want, err := config.Home()
	if err != nil {
		t.Skip("no home in the password database")
	}
	if got := locate().home; got != want {
		t.Errorf("home %q, want %q", got, want)
	}
	t.Setenv(config.StateDirEnv, t.TempDir())
	if got := locate().home; got != "" {
		t.Errorf("home %q under the override, want none", got)
	}
}

// Each way an inherited hold fails to count is named on enqueue, a closed code and never the
// variable's value. Mutations: any two codes swapped; the value written instead of the code.
func TestNestedMissReason(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string) string{
		"malformed": func(*testing.T, string) string { return "../holds" },
		"not_ancestor": func(t *testing.T, dir string) string {
			holdOther(t, dir, 1)
			return entries(t, dir, "holds")[0]
		},
		"dead": func(t *testing.T, dir string) string {
			dead := wire.HoldName(1, os.Getpid(), 1, "dead") // the test process is an ancestor
			if err := os.WriteFile(filepath.Join(dir, "holds", dead), []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
			return dead
		},
	}
	for want, set := range cases {
		t.Run(want, func(t *testing.T) {
			dir := recordedDir(t, 1024)
			hold := set(t, dir)
			r := cli(t, append(baseEnv(dir), engine.EnvHold+"="+hold), "run", "--restart=yes", "--no-wait", "--mem", "1M", "--", "true")
			if r.code != 0 {
				t.Fatalf("exit %d: %q", r.code, r.stderr)
			}
			if got := event(t, events(t, dir), "enqueue")["nested_miss"]; got != want {
				t.Errorf("nested_miss %v, want %s", got, want)
			}
		})
	}
	// A directory that cannot be resolved fails the run's own location too, so no enqueue says it.
	t.Run("unresolved", func(t *testing.T) {
		env := []string{"CORESLOT_STATE_DIR=state", engine.EnvHold + "=" + wire.HoldName(1, os.Getpid(), 1, "x")}
		if inside, _, miss := engine.DecideNested(env, func(int) bool { return true }); inside || miss != "unresolved" {
			t.Errorf("inside %v, miss %q", inside, miss)
		}
	})
}

// A run with no inherited hold has no miss to name, empty counting as unset. Mutation: a hold
// that does not parse named malformed without looking whether one was set.
func TestTopLevelRunHasNoNestedMiss(t *testing.T) {
	for name, env := range map[string][]string{"unset": nil, "empty": {engine.EnvHold + "="}} {
		t.Run(name, func(t *testing.T) {
			dir := recordedDir(t, 1024)
			if r := cli(t, append(baseEnv(dir), env...), "run", "--restart=yes", "--", "true"); r.code != 0 {
				t.Fatalf("exit %d: %q", r.code, r.stderr)
			}
			m := event(t, events(t, dir), "enqueue")
			if _, ok := m["nested_miss"]; ok {
				t.Errorf("nested_miss %v", m["nested_miss"])
			}
			if _, ok := m["parent_hold"]; ok {
				t.Errorf("parent_hold %v", m["parent_hold"])
			}
		})
	}
}

// An --independent run inside a live parent names the parent on its enqueue. Mutation: leaving
// parent_hold out.
func TestEnqueueNamesIndependentParent(t *testing.T) {
	dir := recordedDir(t, 1024)
	if r := outer(t, dir, "256M", `"$BIN" run --independent --label inner --mem 128M -- true`); r.code != 0 {
		t.Fatalf("exit %d: %q", r.code, r.stderr)
	}
	evs := events(t, dir)
	inner := event(t, labelled(evs, "inner"), "enqueue")
	if want := event(t, evs, "admitted")["hold"]; inner["parent_hold"] != want || want == nil { // the outer run's comes first
		t.Errorf("parent_hold %v, want %v", inner["parent_hold"], want)
	}
}

// Nothing is written after a FORMAT the run cannot read, wherever the run sees it. Mutations:
// the FORMAT check on a failed run-log append reddens "run log", a Stop on any failed append "a
// plain failure"; the Stop in updateHold reddens "hold update"; the one on a tick's refusal "tick".
func TestNoEndedAfterFormatChange(t *testing.T) {
	for name, format := range map[string]string{"unknown": "coreslot-protocol 3\n", "malformed": "coreslot-protocol x\n"} {
		t.Run(name, func(t *testing.T) {
			dir := recordedDir(t, 1024)
			env := append(baseEnv(dir), "S="+dir, "F="+format)
			if r := cli(t, env, "run", "--restart=yes", "--", "sh", "-c", `printf %s "$F" > "$S/FORMAT"`); r.code != 0 {
				t.Fatalf("exit %d: %q", r.code, r.stderr)
			}
			if got := eventNames(events(t, dir)); got != "enqueue admitted" {
				t.Errorf("events %q, want nothing after admitted", got)
			}
		})
	}
	t.Run("run log", func(t *testing.T) {
		b := newEnded(t, "sh", "-c", `printf 'coreslot-protocol 3\n' > "$S/FORMAT"`)
		t.Setenv("S", b.dir)
		if code := b.r.runChild(b.child("", "")); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if got := eventNames(events(t, b.dir)); got != "" {
			t.Errorf("events %q", got)
		}
	})
	t.Run("a plain failure of the run log keeps ended", func(t *testing.T) { // its record is the attempt's only copy
		b := newEnded(t, "true")
		path := filepath.Join(b.dir, "log", "runs.jsonl")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if code := b.r.runChild(b.child("", "")); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if got := eventNames(events(t, b.dir)); got != "ended" {
			t.Errorf("events %q, want ended", got)
		}
	})
	seen := func(t *testing.T, see func(b *bed, g *engine.Grant)) {
		b := newBed(t, 1024)
		g, code, ok := b.wait(t, context.Background())
		if !ok {
			t.Fatalf("not admitted: %d %q", code, b.notices(t))
		}
		defer g.Close()
		before := eventNames(events(t, b.dir))
		setFormat(t, b.dir, "coreslot-protocol 3\n")
		see(b, g)
		setFormat(t, b.dir, "coreslot-protocol 2\n") // the way back does not bring the recorder back
		b.r.rec.Emit("after", nil)
		if got := eventNames(events(t, b.dir)); got != before {
			t.Errorf("events %q, want %q", got, before)
		}
		if !b.r.rec.Stopped() {
			t.Error("the recorder is live")
		}
	}
	t.Run("hold update", func(t *testing.T) {
		seen(t, func(b *bed, g *engine.Grant) {
			if b.r.updateHold(b.e.Store(), g, func(*wire.Hold) {}) {
				t.Error("the hold was updated under a foreign FORMAT")
			}
		})
	})
	t.Run("tick", func(t *testing.T) {
		seen(t, func(b *bed, g *engine.Grant) {
			if _, err := b.r.tick(context.Background(), g); !errors.Is(err, engine.ErrRefused) {
				t.Errorf("tick: %v", err)
			}
		})
	})
}
