//go:build !coreslot_testhooks

package testhooks

import (
	"os"
	"path/filepath"
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
	t.Setenv("CORESLOT_TESTHOOK_GETENT", "/tmp/fake-getent")
	t.Setenv("CORESLOT_TESTHOOK_PASSWD_MISS", "1")
	t.Setenv("CORESLOT_TESTHOOK_PROBE_SHELL", "/nonexistent")
	t.Setenv("CORESLOT_TESTHOOK_PROBE_TIMEOUT", "1m")
	t.Setenv("CORESLOT_TESTHOOK_HEADROOM_MIB", "4096")
	if err := os.WriteFile(filepath.Join(dir, "realtime-offset"), []byte("1h"), 0o600); err != nil {
		t.Fatal(err)
	}

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
	if len(entries) != 1 || entries[0].Name() != "realtime-offset" {
		t.Errorf("Point created files without the tag: %v", entries)
	}
	if got := RealtimeOffset(); got != 0 {
		t.Errorf("RealtimeOffset() = %v, want 0", got)
	}
	if got := Formats(); !slices.Equal(got, []int{1}) {
		t.Errorf("Formats() = %v, want [1]", got)
	}
	if got := Getent(); got != "" {
		t.Errorf("Getent() = %q, want \"\"", got)
	}
	if PasswdMiss() {
		t.Error("PasswdMiss() = true, want false")
	}
	if got := ProbeShell(); got != "/bin/sh" {
		t.Errorf("ProbeShell() = %q, want /bin/sh", got)
	}
	if got := ProbeTimeout(time.Second); got != time.Second {
		t.Errorf("ProbeTimeout(1s) = %v, want 1s", got)
	}
	if mib, set := Headroom(); mib != 0 || set {
		t.Errorf("Headroom() = %d, %v, want 0, false", mib, set)
	}
}
