//go:build unix

package store

import (
	"encoding/json"
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
	calls int
	jobs  []watch.Job
}

func (m *machine) read() (watch.Sample, error) {
	m.calls++
	ns := int64(1e12) + int64(m.calls)*2e9
	s := watch.Sample{Jobs: slices.Clone(m.jobs), RoomMiB: -100}
	s.Host.Decomp, s.Host.WallNS, s.Host.AwakeNS = uint64(m.calls)*1e6, ns, ns
	s.Host.SwapFreeMiB, s.Host.VMVolumeFreeMiB = 8192, 100000
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
	_, s, at, err := sec.Watch()
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
	if d.Level != watch.Red || !slices.Equal(stop, []string{"3"}) {
		t.Fatalf("level %v, stop %v; want red, [3]", d.Level, stop)
	}
	round := d.LastRoundNS
	for range 3 {
		if d, stop = tick(); d.LastRoundNS != round || !slices.Equal(stop, []string{"3"}) {
			t.Fatalf("a second round while 3 runs: %v at %d", stop, d.LastRoundNS)
		}
	}
	m.jobs[2].ChildExited = true
	for d.LastRoundNS == round && m.calls < 40 {
		d, stop = tick()
	}
	if waited := time.Duration(d.LastRoundNS - round); waited < 15*time.Second || !slices.Equal(stop, []string{"2"}) {
		t.Errorf("next round after %v: %v; want [2] after post_kill_s", waited, stop)
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

// The sample log rotates under the run log's rule: at 10 MiB the ticker renames it to watch.1.jsonl
// before it appends. Mutation: appending without the size check grows watch.jsonl past 10 MiB.
func TestSampleLogRotates(t *testing.T) {
	dir := newDir(t)
	sec := enter(t, open(t, dir))
	c := watch.Defaults()
	c.SampleLog = true
	m := &machine{}
	if _, _, err := sec.Tick(NowNS(), m.read, c); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, LogDir, WatchLog)
	if err := os.Truncate(log, RotateRunLogAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sec.Tick(NowNS()+21e8, m.read, c); err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(filepath.Join(dir, LogDir, "watch.1.jsonl"))
	cur, err2 := os.Stat(log)
	if err != nil || err2 != nil || old.Size() != RotateRunLogAt || cur.Size() >= RotateRunLogAt {
		t.Errorf("after the rotation: watch.1.jsonl %v (%v), watch.jsonl %v (%v)", old, err, cur, err2)
	}
}

// With sample_log on, each machine sample is one line of log/watch.jsonl carrying every key of
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
		body, err := os.ReadFile(filepath.Join(dir, LogDir, WatchLog))
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
