//go:build unix

package store

import (
	"slices"
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
		NormalShare: 0.05, Victim: true}
}

// Two runs ticking in one period share one machine sample: the second only reads it. The next
// period's tick samples again. Mutation: sampling at every Tick calls read twice.
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
