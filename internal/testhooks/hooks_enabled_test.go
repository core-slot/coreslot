//go:build coreslot_testhooks

package testhooks

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func waitForFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear within %v", path, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPointPausesUntilReleased(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CORESLOT_TESTHOOK_DIR", dir)
	t.Setenv("CORESLOT_TESTHOOK_PAUSE", "store.before-rename,admit.before-enter")

	// A point that is not listed returns at once and leaves nothing behind.
	Point("admit.after-enter")
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("an unlisted point created files: %v", entries)
	}

	prefix := filepath.Join(dir, fmt.Sprintf("%d.admit.before-enter", os.Getpid()))
	done := make(chan struct{})
	go func() {
		Point("admit.before-enter")
		close(done)
	}()
	waitForFile(t, prefix+".reached", 5*time.Second)
	select {
	case <-done:
		t.Fatal("Point returned before it was released")
	case <-time.After(300 * time.Millisecond):
	}
	if err := os.WriteFile(prefix+".go", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Point did not return after release")
	}
	for _, suffix := range []string{".reached", ".go"} {
		if _, err := os.Stat(prefix + suffix); !os.IsNotExist(err) {
			t.Errorf("%s still exists after release (err %v)", prefix+suffix, err)
		}
	}
}

func TestRealtimeOffsetAndFormatsEnabled(t *testing.T) {
	t.Setenv("CORESLOT_TESTHOOK_REALTIME_OFFSET", "")
	t.Setenv("CORESLOT_TESTHOOK_FORMATS", "")
	t.Setenv("CORESLOT_TESTHOOK_GETENT", "")
	if got := RealtimeOffset(); got != 0 {
		t.Errorf("unset RealtimeOffset() = %v, want 0", got)
	}
	if got := Formats(); !slices.Equal(got, []int{1}) {
		t.Errorf("unset Formats() = %v, want [1]", got)
	}
	if got := Getent(); got != "" {
		t.Errorf("unset Getent() = %q, want \"\"", got)
	}
	t.Setenv("CORESLOT_TESTHOOK_REALTIME_OFFSET", "-90s")
	t.Setenv("CORESLOT_TESTHOOK_FORMATS", "1,2")
	t.Setenv("CORESLOT_TESTHOOK_GETENT", "/tmp/fake-getent")
	if got := RealtimeOffset(); got != -90*time.Second {
		t.Errorf("RealtimeOffset() = %v, want -1m30s", got)
	}
	if got := Formats(); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("Formats() = %v, want [1 2]", got)
	}
	if got := Getent(); got != "/tmp/fake-getent" {
		t.Errorf("Getent() = %q, want /tmp/fake-getent", got)
	}
}

// The offset file under CORESLOT_TESTHOOK_DIR replaces the environment's offset while it exists
// and is re-read whenever it changes, so a test can step a running process's wall clock.
func TestRealtimeOffsetFileSteps(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CORESLOT_TESTHOOK_DIR", dir)
	t.Setenv("CORESLOT_TESTHOOK_REALTIME_OFFSET", "-90s")
	path := filepath.Join(dir, OffsetFile)
	write := func(v string, mtime time.Time) {
		t.Helper()
		tmp := path + ".partial"
		if err := os.WriteFile(tmp, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(tmp, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	if got := RealtimeOffset(); got != -90*time.Second {
		t.Fatalf("without the file RealtimeOffset() = %v, want the environment's -1m30s", got)
	}
	base := time.Now()
	write("1h\n", base)
	if got := RealtimeOffset(); got != time.Hour {
		t.Errorf("RealtimeOffset() = %v, want the file's 1h", got)
	}
	// A step to a value of the same size, distinguished by its mtime alone.
	write("2h\n", base.Add(time.Second))
	if got := RealtimeOffset(); got != 2*time.Hour {
		t.Errorf("after the step RealtimeOffset() = %v, want 2h", got)
	}
	write("-1h\n", base.Add(2*time.Second))
	if got := RealtimeOffset(); got != -time.Hour {
		t.Errorf("after the backward step RealtimeOffset() = %v, want -1h", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := RealtimeOffset(); got != -90*time.Second {
		t.Errorf("after the file is removed RealtimeOffset() = %v, want the environment's -1m30s", got)
	}
	write("nonsense", base.Add(3*time.Second))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a malformed offset file did not panic")
			}
		}()
		RealtimeOffset()
	}()
}
