//go:build !coreslot_testhooks

package testhooks

import (
	"os"
	"slices"
	"testing"
	"time"
)

// Without the build tag every hook is inert, whatever the environment says.
func TestHooksInertWithoutTag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CORESLOT_TESTHOOK_DIR", dir)
	t.Setenv("CORESLOT_TESTHOOK_PAUSE", "admit.before-enter,x")
	t.Setenv("CORESLOT_TESTHOOK_REALTIME_OFFSET", "-5m")
	t.Setenv("CORESLOT_TESTHOOK_FORMATS", "1,2")

	done := make(chan struct{})
	go func() {
		Point("x")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Point blocked without the coreslot_testhooks tag")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Point created %d files without the tag: %v", len(entries), entries[0].Name())
	}
	if got := RealtimeOffset(); got != 0 {
		t.Errorf("RealtimeOffset() = %v, want 0", got)
	}
	if got := Formats(); !slices.Equal(got, []int{1}) {
		t.Errorf("Formats() = %v, want [1]", got)
	}
}
