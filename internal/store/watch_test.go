//go:build unix

package store

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/watch"
)

// machine is a read for Tick: every call is the next 2 s of a machine paging hard, with jobs
// whose system share is far above their normal.
type machine struct {
	calls    int
	jobs     []watch.Job
	live     []string // live holds besides the jobs: ones whose run missed a tick
	swapStep int64    // swap used grows by it per call, with no room left in swap or on the volume
}

func (m *machine) read() (watch.Sample, error) {
	m.calls++
	ns := int64(1e12) + int64(m.calls)*2e9
	s := watch.Sample{Jobs: slices.Clone(m.jobs), ShortMiB: 100, Live: slices.Clone(m.live)}
	for _, j := range m.jobs {
		s.Live = append(s.Live, j.Hold)
	}
	s.Host.Decomp, s.Host.WallNS, s.Host.AwakeNS = uint64(m.calls)*1e6, ns, ns
	s.Host.SwapFreeMiB, s.Host.VMVolumeFreeMiB = 8192, 100000
	if m.swapStep > 0 {
		s.Host.SwapUsedMiB, s.Host.SwapFreeMiB, s.Host.VMVolumeFreeMiB = int64(m.calls)*m.swapStep, 100, 100
	}
	return s, nil
}

func stalled(hold string, arrival int64) watch.Job {
	return watch.Job{Hold: hold, ArrivalNS: arrival, FootprintMiB: 512, DUserNS: 4e8, DSysNS: 16e8,
		NormalShare: 0.05, Learned: true, Victim: true}
}

// Two runs ticking in one period share one machine sample: the second only reads it. The next
// period's tick samples again, from 1.5 s on. Mutation: sampling at every Tick calls read twice.
func TestOneSamplePerPeriod(t *testing.T) {
	sec := enter(t, open(t, newDir(t)))
	m := &machine{}
	now := NowNS()
	for _, at := range []int64{now, now + 5e8} {
		if _, _, err := sec.Tick(at, m.read, watch.Defaults()); err != nil {
			t.Fatal(err)
		}
	}
	if m.calls != 1 {
		t.Fatalf("read %d times in one period, want once", m.calls)
	}
	if _, _, err := sec.Tick(now+21e8, m.read, watch.Defaults()); err != nil || m.calls != 2 {
		t.Errorf("the next period: read %d times, %v; want twice", m.calls, err)
	}
	_, s, at, err := sec.s.ReadWatch()
	if err != nil || at != now+21e8 || s.Host.Decomp != 2e6 {
		t.Errorf("stored sample at %d, decomp %d, %v; want the second, at %d", at, s.Host.Decomp, err, now+21e8)
	}
	// A ticker a hair under a period behind the stored sample takes the next one: ticks every
	// 2 s from any number of runs give periods of 1.5 to 2 s, never 4.
	for _, c := range []struct {
		at    int64
		calls int
	}{{now + 31e8, 2}, {now + 40e8, 3}} {
		if _, _, err := sec.Tick(c.at, m.read, watch.Defaults()); err != nil || m.calls != c.calls {
			t.Errorf("tick at +%d ms: read %d times, %v; want %d", (c.at-now)/1e6, m.calls, err, c.calls)
		}
	}
}

// Red writes one stop round; ticks after it, while the listed child runs or before post_kill_s,
// write no other. Once it has exited and post_kill_s has passed, a machine still red gets the
// next round. Mutation: a round at every red tick moves last_round_ns at the next tick.
func TestOneStopRoundPerEpisode(t *testing.T) {
	sec := enter(t, open(t, newDir(t)))
	m := &machine{jobs: []watch.Job{stalled("1", 1), stalled("2", 2), stalled("3", 3)}}
	c := watch.Defaults()
	now := NowNS()
	tick := func() (watch.Detector, []string) {
		t.Helper()
		now += 2e9
		d, stop, err := sec.Tick(now, m.read, c)
		if err != nil {
			t.Fatal(err)
		}
		return d, stop
	}
	d, stop := tick()
	for d.Level != watch.Red && m.calls < 20 {
		d, stop = tick()
	}
	if d.Level != watch.Red || !slices.Equal(stop, []string{"3"}) || d.RoundFloor {
		t.Fatalf("level %v, stop %v, floor round %v; want red, [3], a red round", d.Level, stop, d.RoundFloor)
	}
	round := d.LastRoundNS
	for range 3 {
		if d, stop = tick(); d.LastRoundNS != round || !slices.Equal(stop, []string{"3"}) {
			t.Fatalf("a second round while 3 runs: %v at %d", stop, d.LastRoundNS)
		}
	}
	m.jobs = m.jobs[:2] // the listed hold is gone from the running jobs
	for d.LastRoundNS == round && m.calls < 40 {
		d, stop = tick()
	}
	if waited := time.Duration(d.LastRoundNS - round); waited < 15*time.Second || !slices.Equal(stop, []string{"2"}) {
		t.Errorf("next round after %v: %v; want [2] after post_kill_s", waited, stop)
	}
}

// A listed hold that is no victim any more, a run that found a container client in its tree after
// the round, ignores the stop and may run for hours: it holds back no next round. Mutation: count
// every listed running job in roundOver, as before → no next round, red.
func TestClearedVictimHoldsBackNoRound(t *testing.T) {
	sec := enter(t, open(t, newDir(t)))
	m := &machine{jobs: []watch.Job{stalled("1", 1), stalled("2", 2), stalled("3", 3)}}
	now := NowNS()
	var d watch.Detector
	var stop []string
	for d.LastRoundNS == 0 && m.calls < 20 {
		now += 2e9
		d, stop, _ = sec.Tick(now, m.read, watch.Defaults())
	}
	if !slices.Equal(stop, []string{"3"}) {
		t.Fatalf("first round: stop %v; want [3]", stop)
	}
	round := d.LastRoundNS
	m.jobs[2].Victim = false // 3 runs on, cleared
	for d.LastRoundNS == round && m.calls < 40 {
		now += 2e9
		d, stop, _ = sec.Tick(now, m.read, watch.Defaults())
	}
	if !slices.Equal(stop, []string{"2"}) {
		t.Errorf("next round: stop %v at %d, after %d; want [2]", stop, d.LastRoundNS, round)
	}
}

// A floor round lists a learning job that a red round skips, and is recorded as a floor round.
// Mutations: a round that never takes the level's floor lists nothing here; one that does not
// record its kind is not a floor round.
func TestFloorRoundListsLearningJobs(t *testing.T) {
	sec := enter(t, open(t, newDir(t)))
	learning := stalled("2", 2)
	learning.Learned = false
	m := &machine{jobs: []watch.Job{stalled("1", 1), learning}, swapStep: 300}
	now := NowNS()
	var d watch.Detector
	var stop []string
	for d.Level != watch.Floor && m.calls < 20 {
		now += 2e9
		d, stop, _ = sec.Tick(now, m.read, watch.Defaults())
	}
	if d.Level != watch.Floor || !slices.Equal(stop, []string{"2"}) || !d.RoundFloor {
		t.Errorf("level %v, stop %v, floor round %v; want floor, [2], a floor round", d.Level, stop, d.RoundFloor)
	}
}

// A red round keeps a nearly finished job and stores the record in watch.json, so that the next
// round, whoever takes it, compares with it, also after a sample in which the job's run missed its
// tick: once the job's estimate stops shrinking, that round lists it. Mutations: not storing the
// record, or pruning it by the running jobs, keeps the job again as if first seen.
func TestKeptRecordOutlivesTheRound(t *testing.T) {
	st := open(t, newDir(t))
	sec := enter(t, st)
	near := stalled("3", 3)
	near.CPUNS, near.TypicalCPUS, near.TypicalS = 8e9, 10, 50
	m := &machine{jobs: []watch.Job{stalled("1", 1), stalled("2", 2), near}}
	now := NowNS()
	var d watch.Detector
	var stop []string
	for d.LastRoundNS == 0 && m.calls < 20 {
		now += 2e9
		d, stop, _ = sec.Tick(now, m.read, watch.Defaults())
	}
	stored, _, _, err := st.ReadWatch()
	if k := stored.Kept["3"]; err != nil || !slices.Equal(stop, []string{"2"}) || k.RoundNS != d.LastRoundNS || k.DeadlineNS <= k.RoundNS {
		t.Fatalf("first round: stop %v, stored kept %v (%v); want [2], 3 kept at the round with a deadline", stop, stored.Kept, err)
	}
	m.jobs, m.live = []watch.Job{stalled("1", 1)}, []string{"3"} // 2 is gone; 3's run misses a tick
	now += 2e9
	if d, _, _ = sec.Tick(now, m.read, watch.Defaults()); d.Kept["3"].RoundNS == 0 {
		t.Fatalf("a sample without 3's tick dropped its record: %v", d.Kept)
	}
	m.jobs, m.live = []watch.Job{stalled("1", 1), near}, nil // 3's estimate stays where it was
	round := d.LastRoundNS
	for d.LastRoundNS == round && m.calls < 40 {
		now += 2e9
		d, stop, _ = sec.Tick(now, m.read, watch.Defaults())
	}
	if !slices.Equal(stop, []string{"3"}) {
		t.Errorf("next round: stop %v, kept %v; want [3]", stop, d.Kept)
	}
}

// A round written in observe mode, whose listed children never exit, does not hold back the next
// round once the mode is enforce. Mutation: one rule for both modes writes no second round.
func TestObserveRoundDoesNotBlockEnforce(t *testing.T) {
	sec := enter(t, open(t, newDir(t)))
	m := &machine{jobs: []watch.Job{stalled("1", 1), stalled("2", 2), stalled("3", 3)}}
	c := watch.Defaults()
	c.Observe = true
	now := NowNS()
	var d watch.Detector
	for d.LastRoundNS == 0 && m.calls < 20 {
		now += 2e9
		d, _, _ = sec.Tick(now, m.read, c)
	}
	round := d.LastRoundNS
	c.Observe = false
	now += 2e9
	if d, _, err := sec.Tick(now, m.read, c); err != nil || round == 0 || d.LastRoundNS == round {
		t.Errorf("rounds at %d then %d (%v): want an enforced round at the first tick after the switch", round, d.LastRoundNS, err)
	}
}

// samplesLines reads telemetry/samples.jsonl, one decoded map per line.
func samplesLines(t *testing.T, dir string) []map[string]any {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, TelemetryDir, SamplesFile))
	if err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		lines = append(lines, v)
	}
	return lines
}

// A sample line lives in telemetry/, carries what admission read and when, by the epoch clock, and
// leaves log/ and the body of watch.json as they were. Mutations: drop a `json:"-"` on Sample and
// watch.json gains a key; append to LogDir and log/ gains the file.
func TestSampleLogPathAndFields(t *testing.T) {
	dir := newDir(t)
	sec := enter(t, open(t, dir))
	c := watch.Defaults()
	c.SampleLog = true
	m := &machine{jobs: []watch.Job{stalled("1", 1), stalled("2", 2)}, live: []string{"3"}}
	read := func() (watch.Sample, error) {
		s, err := m.read()
		s.HeadroomMiB, s.ReserveMiB, s.HeadroomOK = 5000, 1024, true
		return s, err
	}
	now := NowNS()
	if _, _, err := sec.Tick(now, read, c); err != nil {
		t.Fatal(err)
	}
	lines := samplesLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1", len(lines))
	}
	want := map[string]any{"v": float64(1), "time_ns": float64(now), "live_holds": float64(3),
		"headroom_mib": float64(5000), "headroom_ok": true, "reserve_mib": float64(1024), "t_ns": float64(int64(1e12) + 2e9)}
	for k, v := range want {
		if lines[0][k] != v {
			t.Errorf("%s = %v, want %v", k, lines[0][k], v)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(dir, LogDir)); err == nil && len(entries) != 0 {
		t.Errorf("log/ holds %v, want nothing", entries)
	}
	body, err := os.ReadFile(filepath.Join(dir, WatchFile))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Sample map[string]any `json:"sample"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	keys := slices.Sorted(maps.Keys(state.Sample))
	if !slices.Equal(keys, []string{"host", "jobs", "live", "room_mib", "short_mib"}) {
		t.Errorf("watch.json's sample has keys %v", keys)
	}
}

// headroom_ok says whether the number is a reading: a measured 0 and an unread one write the same
// number. Mutations: "headroom_ok" always true, or inferred from a non-zero headroom.
func TestHeadroomUnknownSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{{"unread", false}, {"measured zero", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newDir(t)
			sec := enter(t, open(t, dir))
			c := watch.Defaults()
			c.SampleLog = true
			m := &machine{}
			read := func() (watch.Sample, error) {
				s, err := m.read()
				s.HeadroomMiB, s.HeadroomOK = 0, tc.ok
				return s, err
			}
			if _, _, err := sec.Tick(NowNS(), read, c); err != nil {
				t.Fatal(err)
			}
			l := samplesLines(t, dir)[0]
			if l["headroom_ok"] != tc.ok || l["headroom_mib"] != float64(0) {
				t.Errorf("headroom_ok %v, headroom_mib %v; want %v, 0", l["headroom_ok"], l["headroom_mib"], tc.ok)
			}
		})
	}
}

// Each tick rotates both files at the bound, each to one older generation; one under it stays.
// Mutations: rotate only samples.jsonl; compare with > and the file at the bound stays; a second
// generation kept, or a bound other than 256 MiB.
func TestTelemetryFilesRotateAtTheBound(t *testing.T) {
	if rotateTelemetryAt != 256<<20 {
		t.Fatalf("bound %d, want 256 MiB", rotateTelemetryAt)
	}
	const bound = 64
	rotateTelemetryAt = bound
	t.Cleanup(func() { rotateTelemetryAt = 256 << 20 })
	dir := newDir(t)
	sec := enter(t, open(t, dir))
	c := watch.Defaults()
	c.SampleLog = true
	m := &machine{}
	now := NowNS()
	read := func(name string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(dir, TelemetryDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	tick := func() {
		t.Helper()
		now += 21e8
		if _, _, err := sec.Tick(now, m.read, c); err != nil {
			t.Fatal(err)
		}
	}
	events := func(size int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, TelemetryDir, EventsFile), []byte(strings.Repeat("e", size)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	first := read(SamplesFile) // a sample line is longer than the bound
	events(bound - 1)
	tick()
	second := read(SamplesFile)
	if read(SamplesOld) != first || second == first {
		t.Errorf("after the rotation: old %q, current %q; want the first line moved", read(SamplesOld), second)
	}
	if got := read(EventsFile); len(got) != bound-1 {
		t.Errorf("events.jsonl under the bound is %d bytes, want it untouched", len(got))
	}
	if exists(filepath.Join(dir, TelemetryDir, EventsOld)) {
		t.Error("events.jsonl under the bound was rotated")
	}
	events(bound)
	tick()
	if got := read(EventsOld); len(got) != bound {
		t.Errorf("events.1.jsonl is %d bytes, want the file at the bound", len(got))
	}
	if exists(filepath.Join(dir, TelemetryDir, EventsFile)) {
		t.Error("events.jsonl stayed at the bound")
	}
	if read(SamplesOld) != second {
		t.Errorf("samples.1.jsonl = %q, want only the newest older generation %q", read(SamplesOld), second)
	}
}

// A label's C1 controls are escaped on a sample line, as on an event line. Mutation: json.Marshal
// for the encoder leaves U+0085 raw, which a reader that splits on it takes for a line's end.
func TestSampleLineEscapesLabels(t *testing.T) {
	dir := newDir(t)
	sec := enter(t, open(t, dir))
	c := watch.Defaults()
	c.SampleLog = true
	m := &machine{jobs: []watch.Job{{Hold: "h", Label: "a\u0085b"}}}
	if _, _, err := sec.Tick(NowNS(), m.read, c); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, TelemetryDir, SamplesFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "\u0085") {
		t.Errorf("a raw U+0085 in %q", body)
	}
}

// With sample_log on, each machine sample is one line of telemetry/samples.jsonl carrying every key of
// the calibration's schema, and the sample that wrote a stop round names it; with it off nothing
// is written. Mutation: logging at every Tick writes two lines for the first period.
func TestSampleLog(t *testing.T) {
	top := []string{"t_ns", "awake_ns", "level", "excess", "bucket", "gate", "decomp_s", "swapins_s",
		"psi_some_us_s", "swap_used_mib", "swap_free_mib", "vm_free_mib", "room_mib", "jobs"}
	perJob := []string{"hold", "label", "arrival_ns", "need_mib", "footprint_mib", "share", "normal_share",
		"d_user_ns", "d_sys_ns", "d_page_wait_ns"}
	run := func(c watch.Config) (lines []map[string]any, dir string) {
		dir = newDir(t)
		sec := enter(t, open(t, dir))
		m := &machine{jobs: []watch.Job{stalled("1", 1), stalled("2", 2), {Hold: "idle", ArrivalNS: 3}}}
		now := NowNS()
		for _, at := range []int64{now, now + 5e8, now + 21e8} { // the first two share a period
			if _, _, err := sec.Tick(at, m.read, c); err != nil {
				t.Fatal(err)
			}
		}
		for m.calls < 20 {
			now += 21e8
			if _, _, err := sec.Tick(now, m.read, c); err != nil {
				t.Fatal(err)
			}
		}
		body, err := os.ReadFile(filepath.Join(dir, TelemetryDir, SamplesFile))
		if err != nil {
			return nil, dir
		}
		for _, l := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			var v map[string]any
			if err := json.Unmarshal([]byte(l), &v); err != nil {
				t.Fatalf("line %q: %v", l, err)
			}
			lines = append(lines, v)
		}
		return lines, dir
	}

	c := watch.Defaults()
	if lines, _ := run(c); lines != nil {
		t.Fatalf("sample_log off wrote %d lines", len(lines))
	}
	c.SampleLog = true
	lines, _ := run(c)
	if len(lines) != 20 {
		t.Fatalf("%d lines for 20 samples, want 20", len(lines))
	}
	stops := 0
	for i, l := range lines {
		for _, k := range top {
			if _, ok := l[k]; !ok {
				t.Fatalf("line %d lacks %q: %v", i, k, l)
			}
		}
		jobs := l["jobs"].([]any)
		if len(jobs) != 3 {
			t.Fatalf("line %d has %d jobs, want 3", i, len(jobs))
		}
		for _, k := range perJob {
			if _, ok := jobs[0].(map[string]any)[k]; !ok {
				t.Fatalf("line %d: a job lacks %q: %v", i, k, jobs[0])
			}
		}
		if jobs[2].(map[string]any)["share"] != nil || l["psi_some_us_s"] != nil {
			t.Errorf("line %d: a job with no time or a platform without PSI has a number: %v", i, l)
		}
		if s, ok := l["stop"]; ok {
			stops++
			if l["level"] != "red" || !slices.Equal(s.([]any), []any{"2"}) {
				t.Errorf("line %d: stop %v at level %v, want [2] at red", i, s, l["level"])
			}
		}
	}
	if stops != 1 {
		t.Errorf("%d lines name a stop round, want 1", stops)
	}
}

// roundScene is a detector whose last round was at 100 s on the sample clock, with hold "1" listed.
func roundScene() (watch.Detector, watch.Config) {
	c := watch.Defaults()
	c.PostKillS = 15
	return watch.Detector{LastRoundNS: 100e9, Stop: []string{"1"}}, c
}

func sampleAt(wallNS int64, jobs ...watch.Job) watch.Sample {
	var s watch.Sample
	s.Host.WallNS, s.Jobs = wallNS, jobs
	return s
}

// A new round waits out post_kill_s after the last one, and a sample from before it (another
// clock epoch) does not. Mutations: dropping the window clause lets a round start inside it;
// dropping `since >= 0` holds back a sample that reads before the last round.
func TestRoundOverPostKillWindow(t *testing.T) {
	d, c := roundScene()
	for _, tc := range []struct {
		wallNS int64
		want   bool
	}{{100e9, false}, {114e9, false}, {115e9, true}, {99e9, true}} {
		if got, _ := roundOver(d, sampleAt(tc.wallNS), c); got != tc.want {
			t.Errorf("sample at %d s, last round at 100 s: roundOver = %v, want %v", tc.wallNS/1e9, got, tc.want)
		}
	}
}

// A listed hold still running as a victim holds back the next round; one that is gone, no victim
// or not listed does not. Mutations: dropping the victim test, the membership test or the whole
// clause.
func TestRoundOverListedAlive(t *testing.T) {
	d, c := roundScene()
	for _, tc := range []struct {
		name string
		job  watch.Job
		want bool
	}{
		{"listed and a victim", watch.Job{Hold: "1", Victim: true}, false},
		{"listed, no victim any more", watch.Job{Hold: "1"}, true},
		{"a victim, not listed", watch.Job{Hold: "2", Victim: true}, true},
	} {
		if got, _ := roundOver(d, sampleAt(120e9, tc.job), c); got != tc.want {
			t.Errorf("%s: roundOver = %v, want %v", tc.name, got, tc.want)
		}
	}
	if ok, _ := roundOver(d, sampleAt(120e9), c); !ok {
		t.Error("the listed hold is gone: roundOver = false, want true")
	}
}

// A round observe mode wrote holds back no round under enforce, inside the window and with its
// victim alive; under observe it still does. Mutation: drop the first clause of roundOver.
func TestRoundOverObservedRound(t *testing.T) {
	d, c := roundScene()
	d.Observed = true
	cur := sampleAt(105e9, watch.Job{Hold: "1", Victim: true})
	if ok, _ := roundOver(d, cur, c); !ok {
		t.Error("enforce after an observed round: roundOver = false, want true")
	}
	c.Observe = true
	if ok, _ := roundOver(d, cur, c); ok {
		t.Error("observe after an observed round: roundOver = true, want false")
	}
}

// A red sample with no round says why: the post_kill window, a listed hold still running, or a
// round that found nobody to stop. Mutations: the reason left empty; the two refusals swapped.
func TestSampleSaysWhyNoRound(t *testing.T) {
	c := watch.Defaults()
	c.SampleLog = true
	ticks := func(jobs ...watch.Job) (lines []map[string]any, reasons []string) {
		dir := newDir(t)
		sec := enter(t, open(t, dir))
		m := &machine{jobs: jobs}
		now := NowNS()
		for range 30 { // a minute on the sample clock: well past post_kill_s
			now += 2e9
			if _, _, err := sec.Tick(now, m.read, c); err != nil {
				t.Fatal(err)
			}
		}
		lines = samplesLines(t, dir)
		for _, l := range lines {
			if v, ok := l["no_round"].(string); ok {
				reasons = append(reasons, v)
			}
		}
		return lines, reasons
	}
	_, reasons := ticks(stalled("1", 1), stalled("2", 2), stalled("3", 3))
	if len(reasons) < 2 || reasons[0] != "post_kill" || reasons[len(reasons)-1] != "listed_alive" {
		t.Errorf("reasons %v, want post_kill first and listed_alive last", reasons)
	}
	for _, r := range reasons {
		if r != "post_kill" && r != "listed_alive" {
			t.Errorf("reason %q with a victim running", r)
		}
	}
	lines, reasons := ticks(stalled("1", 1)) // the oldest job is never stopped
	if len(reasons) == 0 || reasons[0] != "nobody_stoppable" {
		t.Errorf("reasons %v, want nobody_stoppable", reasons)
	}
	for i, l := range lines {
		if _, stop := l["stop"]; stop && l["no_round"] != nil {
			t.Errorf("line %d names a stop and no round: %v", i, l)
		}
	}
}

// A round's sample line names the candidates, the holds that round spared and the shortage; a
// record of an older round is not this round's. Mutation: listing every d.Kept entry.
func TestSampleKeptIsThisRounds(t *testing.T) {
	dir := newDir(t)
	sec := enter(t, open(t, dir))
	c := watch.Defaults()
	c.SampleLog = true
	near := stalled("3", 3)
	near.CPUNS, near.TypicalCPUS, near.TypicalS = 8e9, 10, 50
	m := &machine{jobs: []watch.Job{stalled("1", 1), stalled("2", 2), near}}
	now := NowNS()
	var d watch.Detector
	for d.LastRoundNS == 0 && m.calls < 20 {
		now += 2e9
		d, _, _ = sec.Tick(now, m.read, c)
	}
	m.jobs = []watch.Job{stalled("1", 1), near} // 2 is gone, 3 is stopped by the next round
	round := d.LastRoundNS
	for d.LastRoundNS == round && m.calls < 40 {
		now += 2e9
		d, _, _ = sec.Tick(now, m.read, c)
	}
	var rounds []map[string]any
	for _, l := range samplesLines(t, dir) {
		if _, ok := l["stop"]; ok {
			rounds = append(rounds, l)
		}
	}
	if len(rounds) != 2 {
		t.Fatalf("%d round lines, want 2", len(rounds))
	}
	if k, _ := rounds[0]["kept"].([]any); len(k) != 1 || k[0] != "3" || rounds[0]["short_mib"] != 100.0 {
		t.Errorf("first round: kept %v short_mib %v, want [3] and 100", rounds[0]["kept"], rounds[0]["short_mib"])
	}
	if c, _ := rounds[0]["candidates"].([]any); len(c) != 3 {
		t.Errorf("candidates %v, want the three victims", rounds[0]["candidates"])
	}
	if k, ok := rounds[1]["kept"]; ok {
		t.Errorf("second round: kept %v, but the record is the first round's", k)
	}
}

// A sample line carries the parts the seam gives and the pressure level when there is one; a read
// that fails leaves the keys out and the line in, and with the sample log off the seam is not
// called. Mutations: calling Parts before the SampleLog test turns the off row red; writing the
// keys when ok is false turns the failed row red.
func TestSampleLogParts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		on, ok   bool
		pressure int
		calls    int
	}{{"on", true, true, 2, 1}, {"failed read", true, false, 0, 1}, {"no pressure", true, true, 0, 1}, {"off", false, true, 1, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			saved := Parts
			Parts = func() (map[string]int64, int, bool) {
				calls++
				if !tc.ok {
					return map[string]int64{"wired": 9}, 4, false
				}
				return map[string]int64{"wired": 9, "oom_kill": 2}, tc.pressure, true
			}
			t.Cleanup(func() { Parts = saved })
			dir := newDir(t)
			sec := enter(t, open(t, dir))
			c := watch.Defaults()
			c.SampleLog = tc.on
			m := &machine{jobs: []watch.Job{stalled("1", 1)}}
			if _, _, err := sec.Tick(NowNS(), m.read, c); err != nil {
				t.Fatal(err)
			}
			if calls != tc.calls {
				t.Errorf("Parts called %d times, want %d", calls, tc.calls)
			}
			if !tc.on {
				return
			}
			l := samplesLines(t, dir)
			if len(l) != 1 {
				t.Fatalf("%d lines, want 1", len(l))
			}
			_, hasWired := l[0]["wired"]
			_, hasPressure := l[0]["pressure_level"]
			if hasWired != tc.ok || hasPressure != (tc.ok && tc.pressure != 0) {
				t.Errorf("wired %v, pressure_level %v in %v", hasWired, hasPressure, l[0])
			}
			if tc.ok && (l[0]["wired"] != float64(9) || l[0]["oom_kill"] != float64(2)) {
				t.Errorf("parts not copied: %v", l[0])
			}
		})
	}
}
