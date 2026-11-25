//go:build unix

package main

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// followRun is a run whose hold follows what it uses over a window of window seconds, its trim
// window trimS: the harness's run keeps the bound at 0, which no grant fits, so the test opens it.
func followRun(t *testing.T, window, trimS int64) *run {
	t.Helper()
	r := trimRun(t, trimS)
	r.windowS, r.boundMiB = window, math.MaxInt64
	return r
}

// A following hold reserves the need asked until its window is full, then the largest reading in it
// plus the margin (400 for 4000), with asked_mib in every write and no trimmed. Mutation: following
// from the first reading (a window of 1) gives 600 at the first tick.
func TestFollowLowersAfterWindow(t *testing.T) {
	_, g, st := trimHold(t, 4000)
	bodies, _ := trimWatch(t, followRun(t, 4, 60), g, st, []int64{3000, 200, 200, 200, 200, 200}, nil)
	need, trimmed, asked := trimOf(bodies)
	if !slices.Equal(need, []int64{4000, 3400, 600}) || slices.Contains(trimmed, true) || !slices.Equal(asked, []int64{4000, 4000, 4000}) {
		t.Errorf("need %v trimmed %v asked %v; want 4000 3400 600, untrimmed, asked 4000", need, trimmed, asked)
	}
}

// Listings that fail add no reading: the window does not age and the reservation holds. Mutation:
// adding 0 for a failed listing gives 400 at the second tick.
func TestFollowKeepsNeedThroughFailedListings(t *testing.T) {
	_, g, st := trimHold(t, 4000)
	bodies, _ := trimWatch(t, followRun(t, 2, 60), g, st, []int64{200, 3000, -1, -1, 200, 200}, nil)
	if need, _, _ := trimOf(bodies); !slices.Equal(need, []int64{3400, 3400, 600}) {
		t.Errorf("need %v; want 3400 3400 600", need)
	}
}

// The reservation rises with the footprint at the next tick, up to the need asked. Mutation: taking
// the window's oldest reading instead of its largest gives 600 at the second tick.
func TestFollowRisesAtOnce(t *testing.T) {
	_, g, st := trimHold(t, 4000)
	bodies, _ := trimWatch(t, followRun(t, 2, 60), g, st, []int64{200, 200, 200, 3900}, nil)
	if need, _, _ := trimOf(bodies); !slices.Equal(need, []int64{600, 4000}) {
		t.Errorf("need %v; want 600 4000", need)
	}
}

// A run follows only on a learned need, without a Docker interest, within the bound, on its first
// attempt: otherwise its hold keeps the need asked and writes no asked_mib, and a trim still
// follows trim's window of 2. One mutation per case: dropping its clause from follows lowers the
// untrimmed need, red. A container command alone is the exception: it is container work from its
// first tick, so the reset to the need asked covers it even without its clause.
func TestNoFollow(t *testing.T) {
	for _, c := range []struct {
		name string
		argv []string
		set  func(r *run)
	}{
		{"WhenPinned", nil, func(r *run) { r.o.pinned = true }}, // --mem and the needs file's pin alike
		{"WithContainerCommand", []string{"docker", "run", "x"}, nil},
		{"WithMarkedLabel", nil, func(r *run) { r.marked = true }},
		{"WithDockerMem", nil, func(r *run) { r.entry.DockerMemMiB = 1024 }},
		{"WhenOversized", nil, func(r *run) { r.boundMiB = 3999 }},
		{"OnRestart", nil, func(r *run) { r.stops = 1 }}, // the harness's requeue does not count stops
	} {
		for _, trim := range []bool{false, true} {
			t.Run(c.name+map[bool]string{false: "", true: "Trimmed"}[trim], func(t *testing.T) {
				e, g, st := trimHold(t, 4000)
				r := followRun(t, 4, 2)
				if c.argv != nil {
					r.o.argv = c.argv
				}
				if c.set != nil {
					c.set(r)
				}
				each := func(n int) {
					if n == 1 && trim {
						requestTrim(t, e, g)
					}
				}
				bodies, _ := trimWatch(t, r, g, st, []int64{3000, 200, 200, 200, 200, 200}, each)
				need, _, asked := trimOf(bodies)
				wantNeed, wantAsked := []int64{4000, 4000, 4000}, []int64{0, 0, 0}
				if trim {
					wantNeed, wantAsked = []int64{3400, 600, 600}, []int64{4000, 4000, 4000}
				}
				if !slices.Equal(need, wantNeed) || !slices.Equal(asked, wantAsked) {
					t.Errorf("need %v asked %v; want %v and %v", need, asked, wantNeed, wantAsked)
				}
			})
		}
	}
}

// A following run that becomes container work writes the need asked and drops asked_mib at its
// next tick, and keeps doing so. Mutation: following on gives 600 after the client appears.
func TestFollowStopsOnContainerWork(t *testing.T) {
	_, g, st := trimHold(t, 4000)
	each := func(n int) {
		if n == 5 {
			listClient = func(int) bool { return true }
		}
	}
	bodies, _ := trimWatch(t, followRun(t, 2, 60), g, st, []int64{3000, 200, 200, 200, 200, 200, 200, 200}, each)
	need, _, asked := trimOf(bodies)
	if !slices.Equal(need, []int64{3400, 600, 4000, 4000}) || !slices.Equal(asked, []int64{4000, 4000, 0, 0}) {
		t.Errorf("need %v asked %v; want 3400 600 4000 4000, asked 4000 4000 0 0", need, asked)
	}
}

// The reset to the need asked is written in every later update, so one that failed repeats: the
// first write after the client appears fails (FORMAT is not understood for that tick), the next
// writes 4000. Mutation: writing the reset once leaves 600.
func TestContainerResetRepeatsAfterFailedWrite(t *testing.T) {
	_, g, st := trimHold(t, 4000)
	format := filepath.Join(st.Dir(), "FORMAT")
	saved, err := os.ReadFile(format)
	if err != nil {
		t.Fatal(err)
	}
	each := func(n int) {
		switch n {
		case 5:
			listClient = func(int) bool { return true }
			if err := os.WriteFile(format, []byte("99\n"), 0o600); err != nil {
				t.Error(err)
			}
		case 7:
			if err := os.WriteFile(format, saved, 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	bodies, _ := trimWatch(t, followRun(t, 2, 60), g, st, []int64{3000, 200, 200, 200, 200, 200, 200, 200}, each)
	if need, _, _ := trimOf(bodies); !slices.Equal(need, []int64{3400, 600, 600, 4000}) {
		t.Errorf("need %v; want 3400 600 600 4000", need)
	}
}

// A run that followed over 8 readings and became container work, then trimmed, follows trim's 2.
// Mutation: shortening the window only while the run follows leaves it at 8, not yet full: 4000.
func TestTrimAfterFollowEndsUsesTrimWindow(t *testing.T) {
	e, g, st := trimHold(t, 4000)
	each := func(n int) {
		switch n {
		case 3:
			listClient = func(int) bool { return true }
		case 5:
			requestTrim(t, e, g)
		}
	}
	bodies, _ := trimWatch(t, followRun(t, 8, 2), g, st, []int64{3000, 3000, 3000, 3000, 200, 200}, each)
	need, trimmed, _ := trimOf(bodies)
	if !slices.Equal(need, []int64{4000, 4000, 600}) || !slices.Equal(trimmed, []bool{false, false, true}) {
		t.Errorf("need %v trimmed %v; want 4000 4000 600, trimmed at the last", need, trimmed)
	}
}

// A trim makes a following hold's window the shorter of the two: a window of 8 trimmed to 2
// follows 2; a window of 2 trimmed to 8 stays 2. Mutation: taking the trim window as it is (a new
// window of the trim's length) gives 4000 at the first tick of both.
func TestTrimShortensFollowWindow(t *testing.T) {
	for _, c := range []struct{ window, trimS int64 }{{8, 2}, {2, 8}} {
		e, g, st := trimHold(t, 4000)
		each := func(n int) {
			if n == 1 {
				requestTrim(t, e, g)
			}
		}
		bodies, _ := trimWatch(t, followRun(t, c.window, c.trimS), g, st, []int64{3000, 200, 200, 200}, each)
		if need, _, _ := trimOf(bodies); !slices.Equal(need, []int64{3400, 600}) {
			t.Errorf("window %d, trim %d: need %v; want 3400 600", c.window, c.trimS, need)
		}
	}
}
