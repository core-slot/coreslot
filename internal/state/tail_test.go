package state

import (
	"encoding/binary"
	"math/rand/v2"
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

// gapReference is mem_window_s in readings, computed from its definition in quadratic time.
func gapReference(rs []int32, asked int64) int {
	m, n := Margin(asked), 0
	for j := 1; j < len(rs); j++ { // reading j in the definition's numbering, rs[j:] the later ones
		var f int64
		for _, r := range rs[j:] {
			f = max(f, int64(r))
		}
		u := 0
		for i := j; i >= 1; i-- {
			if int64(rs[i-1]) >= f-m {
				u = i
				break
			}
		}
		n = max(n, j-u+1)
	}
	return n
}

// covers reports whether a Tail of window readings, fed the series, never reserves less than a
// later reading that is at most the need asked.
func covers(rs []int32, asked int64, window int) bool {
	tail := NewTail(asked, window)
	for t, r := range rs {
		tail.Add(int64(r))
		for _, later := range rs[t+1:] {
			if int64(later) <= asked && tail.Need() < int64(later) {
				return false
			}
		}
	}
	return true
}

// TestGapCounterExample: a slow climb after a low stretch counts in full, looking forward to the
// largest reading still to come; a window one reading shorter reserves too little. Mutation:
// looking back from each reading to the largest earlier one gives 1.
func TestGapCounterExample(t *testing.T) {
	rs := []int32{100, 100, 100, 400, 700, 1000, 1300, 1600}
	if n, ok := Gap(rs, 4000); n != 7 || !ok {
		t.Fatalf("Gap = %d, %v; want 7, true", n, ok)
	}
	if !covers(rs, 4000, 7) || covers(rs, 4000, 6) {
		t.Errorf("a window of 7 must cover the series and one of 6 must not")
	}
}

// TestGapShapes: a climb of k readings, each step above the margin, gives k; a dip of d readings
// between equal peaks gives d + 1; a flat series gives 1; one reading gives none. Mutation:
// dropping the + 1 gives 0 for the flat series.
func TestGapShapes(t *testing.T) {
	for _, c := range []struct {
		rs   []int32
		want int
		ok   bool
	}{
		{[]int32{0, 500, 1000, 1500}, 4, true},
		{[]int32{3000, 0, 0, 0, 3000}, 4, true},
		{[]int32{800, 800, 800}, 1, true},
		{[]int32{800}, 0, false},
	} {
		if n, ok := Gap(c.rs, 4000); n != c.want || ok != c.ok {
			t.Errorf("Gap(%v) = %d, %v; want %d, %v", c.rs, n, ok, c.want, c.ok)
		}
	}
}

// TestGapLaterPeakLengthensEarlierGap: the 1650 at the end lengthens the gap behind the 0 at
// reading 6 back to the 1300 at reading 1. Mutation: computing the gap only at the newest reading
// gives 2.
func TestGapLaterPeakLengthensEarlierGap(t *testing.T) {
	if n, _ := Gap([]int32{1300, 0, 0, 1000, 0, 0, 1250, 0, 1650}, 4000); n != 6 {
		t.Errorf("Gap = %d; want 6", n)
	}
}

// FuzzGap: for any series of up to 64 readings, Gap equals the definition, and a window of that
// many readings never reserves less than a later reading at most the need asked. Mutation: taking
// the stack's bottom instead of the topmost reading at least F - m gives 5 instead of 3 for the
// second seed (400, 0, 300, 0, 0, 300).
func FuzzGap(f *testing.F) {
	f.Add(uint16(4000), []byte{0, 100, 0, 100, 0, 100, 1, 144, 2, 188, 3, 232, 5, 20, 6, 64})
	f.Add(uint16(0), []byte{1, 144, 0, 0, 1, 44, 0, 0, 0, 0, 1, 44})
	f.Fuzz(func(t *testing.T, asked uint16, data []byte) {
		var rs []int32
		for i := 0; i+2 <= len(data) && len(rs) < 64; i += 2 {
			rs = append(rs, int32(binary.BigEndian.Uint16(data[i:])%8193))
		}
		n, ok := Gap(rs, int64(asked))
		if want := gapReference(rs, int64(asked)); ok != (len(rs) >= 2) || n != want {
			t.Fatalf("Gap(%v, %d) = %d, %v; want %d", rs, asked, n, ok, want)
		}
		if ok && !covers(rs, int64(asked), n) {
			t.Fatalf("a window of %d readings does not cover %v (asked %d)", n, rs, asked)
		}
	})
}

// TestTailShortenKeepsNewest: shortening keeps the newest readings and never lengthens. Mutation:
// keeping the oldest readings (1 and 2) gives 258 after the 0 instead of 261.
func TestTailShortenKeepsNewest(t *testing.T) {
	tail := NewTail(1000, 5)
	addAll(tail, 1, 2, 3, 4, 5)
	tail.Shorten(10)
	if got := tail.Need(); got != 261 {
		t.Fatalf("Need after Shorten(10) = %d; want 261", got)
	}
	tail.Shorten(2)
	tail.Add(0)
	if got := tail.Need(); got != 261 {
		t.Errorf("Need after Shorten(2) and a 0 = %d; want 261 (5 + 256)", got)
	}
	tail.Add(0)
	if got := tail.Need(); got != 256 {
		t.Errorf("Need after a second 0 = %d; want 256", got)
	}
}

// BenchmarkGapDay: a day-long run's readings, one a second, as a random walk.
func BenchmarkGapDay(b *testing.B) {
	rs := make([]int32, 86400)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 1; i < len(rs); i++ {
		rs[i] = max(0, rs[i-1]+int32(rng.IntN(201))-100)
	}
	for b.Loop() {
		Gap(rs, 8000)
	}
}
