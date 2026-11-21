package state

import (
	"encoding/binary"
	"testing"
)

// addAll feeds readings to t and returns Need after each one.
func addAll(t *Tail, readings ...int64) []int64 {
	needs := make([]int64, 0, len(readings))
	for _, r := range readings {
		t.Add(r)
		needs = append(needs, t.Need())
	}
	return needs
}

func equalNeeds(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTailKeepsAskedUntilWindowFull: the need asked holds until the window has as many readings
// as it spans, and with none at all. Mutation: following from the first reading (dropping the
// count check) gives 500 after the first reading instead of 4000.
func TestTailKeepsAskedUntilWindowFull(t *testing.T) {
	tail := NewTail(4000, 3)
	if got := tail.Need(); got != 4000 {
		t.Fatalf("Need with no reading = %d; want 4000", got)
	}
	if got, want := addAll(tail, 100, 100, 100), []int64{4000, 4000, 500}; !equalNeeds(got, want) {
		t.Errorf("needs = %v; want %v", got, want)
	}
}

// TestTailFollowsLargestOfWindow: recent is the largest of the last window readings (margin 400
// for a need of 4000). Mutations: using the newest reading gives 600 after the 200 instead of
// 1300; using the oldest gives 500 there.
func TestTailFollowsLargestOfWindow(t *testing.T) {
	tail := NewTail(4000, 3)
	got := addAll(tail, 100, 900, 200, 150, 150, 150)
	want := []int64{4000, 4000, 1300, 1300, 600, 550}
	if !equalNeeds(got, want) {
		t.Errorf("needs = %v; want %v", got, want)
	}
}

// TestTailMargin: the margin is a tenth of the need asked with a floor of MarginMinMiB.
// Mutation: dropping the floor gives Margin(1000) = 100 and Margin(1) = 0.
func TestTailMargin(t *testing.T) {
	for _, c := range []struct{ asked, want int64 }{{1000, 256}, {11665, 1166}, {1, 256}} {
		if got := Margin(c.asked); got != c.want {
			t.Errorf("Margin(%d) = %d; want %d", c.asked, got, c.want)
		}
	}
}

// TestTailNeverAboveAsked: a footprint at or above the need asked reserves the need asked, never
// more. Mutation: dropping the cap gives 5256 and 1156.
func TestTailNeverAboveAsked(t *testing.T) {
	tail := NewTail(1000, 1)
	if got, want := addAll(tail, 5000, 900), []int64{1000, 1000}; !equalNeeds(got, want) {
		t.Errorf("needs = %v; want %v", got, want)
	}
}

// TestTailRisesAtOnce: a reading counts in the Need that follows its Add, so the reservation
// rises in the same tick as the footprint. Mutation: leaving the newest reading out of recent (the
// largest seen before the Add) gives 500 instead of 3400.
func TestTailRisesAtOnce(t *testing.T) {
	tail := NewTail(4000, 3)
	addAll(tail, 100, 100, 100)
	tail.Add(3000)
	if got := tail.Need(); got != 3400 {
		t.Errorf("Need after a reading of 3000 = %d; want 3400", got)
	}
}

// FuzzTail: for any need asked, window and readings, the need stays within 1 and the need asked,
// and once the window is full it covers the newest reading plus the margin, up to the need asked.
// The first two seeds fill their window (asked 4000 over 3, asked 1 over 1). Mutation: Need
// returning 0 once the window is full → red on the seeds.
func FuzzTail(f *testing.F) {
	f.Add(int64(3999), 2, []byte{0, 0, 0, 100, 0, 0, 3, 132, 0, 0, 0, 200})
	f.Add(int64(0), 0, []byte{255, 255, 255, 255})
	f.Add(int64(1<<30), 120, []byte{})
	f.Fuzz(func(t *testing.T, asked int64, window int, data []byte) {
		asked = 1 + int64(uint64(asked)%(1<<30))
		window = 1 + int(uint(window)%120)
		tail := NewTail(asked, window)
		for i := 0; i+4 <= len(data); i += 4 {
			reading := int64(binary.BigEndian.Uint32(data[i:])) // 0 .. 4 TiB, above any need asked
			tail.Add(reading)
			need := tail.Need()
			if need < 1 || need > asked {
				t.Fatalf("asked %d window %d: Need = %d after reading %d", asked, window, need, reading)
			}
			if full := i/4+1 >= window; full && need < min(asked, reading+Margin(asked)) {
				t.Fatalf("asked %d window %d: Need = %d below the newest reading %d plus its margin",
					asked, window, need, reading)
			}
		}
	})
}
