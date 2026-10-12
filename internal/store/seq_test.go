//go:build unix

package store

import "testing"

// The process's counter follows wire.NextSeq: after 999999 comes 0 (PROTOCOL.md section 7.1).
func TestNextSeqWraps(t *testing.T) {
	old := seq.Load()
	t.Cleanup(func() { seq.Store(old) })
	seq.Store(999998)
	for _, want := range []int{999999, 0, 1} {
		if got := NextSeq(); got != want {
			t.Fatalf("NextSeq() = %d, want %d", got, want)
		}
	}
}
