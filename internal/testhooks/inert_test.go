//go:build !coreslot_testhooks

package testhooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Without the build tag every hook is inert, whatever the environment says.
func TestHooksInertWithoutTag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CORESLOT_TESTHOOK_DIR", dir)
	t.Setenv("CORESLOT_TESTHOOK_PAUSE", "admit.before-enter,x")
	t.Setenv("CORESLOT_TESTHOOK_REALTIME_OFFSET", "-5m")
	t.Setenv("CORESLOT_TESTHOOK_GETENT", "/tmp/fake-getent")
	t.Setenv("CORESLOT_TESTHOOK_PASSWD_MISS", "1")
	t.Setenv("CORESLOT_TESTHOOK_HEADROOM_MIB", "4096")
	for name, v := range map[string]string{"realtime-offset": "1h", "readings": "decomp 1\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
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
	if len(entries) != 2 {
		t.Errorf("Point created files without the tag: %v", entries)
	}
	if got := RealtimeOffset(); got != 0 {
		t.Errorf("RealtimeOffset() = %v, want 0", got)
	}
	if got := Getent(); got != "" {
		t.Errorf("Getent() = %q, want \"\"", got)
	}
	if PasswdMiss() {
		t.Error("PasswdMiss() = true, want false")
	}
	if mib, set := Headroom(); mib != 0 || set {
		t.Errorf("Headroom() = %d, %v, want 0, false", mib, set)
	}
	if got, set := Readings(); got != nil || set {
		t.Errorf("Readings() = %v, %v, want nil, false", got, set)
	}
}
