package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/watch"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMissingFileIsDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent", "config.toml"))
	if err != nil {
		t.Fatalf("Load of a missing file: %v", err)
	}
	if c.ReserveMiB != DefaultReserveMiB {
		t.Errorf("ReserveMiB = %d, want %d", c.ReserveMiB, DefaultReserveMiB)
	}
	if !c.WindowsWarning {
		t.Error("WindowsWarning defaults to false, want true")
	}
}

// An empty file loads as the defaults, and a table or key the configuration does not know is
// ignored. Mutation: reject an unknown table.
func TestConfigMinimal(t *testing.T) {
	for _, body := range []string{"", "[nonesuch]\nkey = 1\n", "[memory]\nfuture = 3\n"} {
		c, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load(%q): %v", body, err)
		}
		if c.ReserveMiB != DefaultReserveMiB || !c.WindowsWarning {
			t.Errorf("Load(%q): reserve %d MiB, windows warning %v; want %d and the warning on", body, c.ReserveMiB, c.WindowsWarning, DefaultReserveMiB)
		}
	}
}

func TestLoadFile(t *testing.T) {
	c, err := Load(writeConfig(t, `
[memory]
reserve = "2G"

[platform]
windows_warning = false

[future-table]
anything = "is ignored"
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.ReserveMiB != 2048 {
		t.Errorf("ReserveMiB = %d, want 2048", c.ReserveMiB)
	}
	if c.WindowsWarning {
		t.Error("windows_warning = false not honoured")
	}
}

// reserve is a size in the rules of spec.ParseBytes or an integer of GiB; 0 is allowed.
// Mutation: reject 0, or read an integer as MiB.
func TestReserveSizes(t *testing.T) {
	for body, want := range map[string]int64{`"768M"`: 768, "2": 2048, `"1.5G"`: 1536, "0": 0, `"0M"`: 0} {
		c, msg := parse("[memory]\nreserve = " + body + "\n")
		if msg != "" {
			t.Errorf("reserve = %s: %s", body, msg)
			continue
		}
		if c.ReserveMiB != want {
			t.Errorf("reserve = %s: %d MiB, want %d", body, c.ReserveMiB, want)
		}
	}
	for _, body := range []string{`"-2"`, "true", "4.5", `"9GB"`, `"lots"`} {
		if _, msg := parse("[memory]\nreserve = " + body + "\n"); msg == "" {
			t.Errorf("reserve = %s accepted", body)
		}
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	bodies := map[string]string{
		"malformed TOML":   "[memory\nreserve = 1\n",
		"duplicate key":    "[memory]\nreserve = 1\nreserve = 2\n",
		"invalid size":     "[memory]\nreserve = \"lots\"\n",
		"float size":       "[memory]\nreserve = 2.5\n",
		"memory not table": "memory = 3\n",
		"warning not bool": "[platform]\nwindows_warning = \"no\"\n",
	}
	for what, body := range bodies {
		_, err := Load(writeConfig(t, body))
		var ce *Error
		if !errors.As(err, &ce) {
			t.Errorf("%s: got %v, want a *config.Error", what, err)
		}
	}
}

// TestMemoryTable pins the shapes of [memory]: a table, with reserve a size.
func TestMemoryTable(t *testing.T) {
	for body, want := range map[string]string{
		"memory = 3\n":                 "[memory] is not a table",
		"[memory]\nreserve = \"no\"\n": `[memory] reserve: "no" is not a size: want a number of GiB, such as 3 or 2.5G, or of MiB, such as 768M`,
	} {
		_, err := Load(writeConfig(t, body))
		var ce *Error
		if !errors.As(err, &ce) || ce.Msg != want {
			t.Errorf("Load(%q) = %v, want a *config.Error %q", body, err, want)
		}
	}
}

// The configuration is read at every call (PROTOCOL.md section 5.3): a replacement that keeps
// the size and the mtime, by rewriting in place or by renaming another file over it, is seen at
// once.
func TestLoadReadsEveryCall(t *testing.T) {
	path := writeConfig(t, "[memory]\nreserve = \"1280M\"\n")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(path); err != nil || c.ReserveMiB != 1280 {
		t.Fatalf("first Load = %v, %v", c.ReserveMiB, err)
	}

	// Renamed over, same size, same mtime (cp -p to a temporary name, then mv).
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte("[memory]\nreserve = \"1536M\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(path); err != nil || c.ReserveMiB != 1536 {
		t.Errorf("Current() after a rename keeping size and mtime: reserve = %d, %v; want 1536", c.ReserveMiB, err)
	}

	// Rewritten in place, same size, same mtime.
	if err := os.WriteFile(path, []byte("[memory]\nreserve = \"1408M\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(path); err != nil || c.ReserveMiB != 1408 {
		t.Errorf("Current() after an in-place rewrite keeping size and mtime: reserve = %d, %v; want 1408", c.ReserveMiB, err)
	}

	// An invalid file is reported as a *config.Error.
	if err := os.WriteFile(path, []byte("[memory]\nreserve = \"lots\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ce *Error
	if _, err := Load(path); !errors.As(err, &ce) {
		t.Errorf("Current() of an invalid file: %v, want a *config.Error", err)
	}

	// Removing the file returns to the defaults.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(path); err != nil || c.ReserveMiB != DefaultReserveMiB {
		t.Errorf("Current() after removal = %v, %v; want defaults", c.ReserveMiB, err)
	}
}

// [watch] sets the mode, the sample log and every threshold; floor is a size as reserve is, and a
// key it does not know is ignored. Without the table the detector's defaults hold.
func TestWatchConfigKeys(t *testing.T) {
	if c, err := Load(writeConfig(t, "")); err != nil || c.Watch != watch.Defaults() {
		t.Errorf("no [watch]: %+v, %v; want the defaults", c.Watch, err)
	}
	c, err := Load(writeConfig(t, `
[watch]
mode = "observe"
sample_log = true
yellow_points = 12
red_points = 35.5
red_samples = 8
gate_pages_s = 500
floor = "3G"
calm_s = 60
post_kill_s = 20
spacing_s = 5
future = "ignored"
`))
	want := watch.Config{YellowPoints: 12, RedPoints: 35.5, RedSamples: 8, GatePagesPerS: 500,
		FloorMiB: 3072, CalmS: 60, PostKillS: 20, SpacingS: 5, Observe: true, SampleLog: true}
	if err != nil || c.Watch != want {
		t.Errorf("[watch]: %+v, %v; want %+v", c.Watch, err, want)
	}
	// A threshold of 0 would make every sample hot, or the floor unreachable; a wait of 0 is none.
	if c, err := Load(writeConfig(t, "[watch]\ncalm_s = 0\npost_kill_s = 0\nspacing_s = 0\n")); err != nil ||
		c.Watch.CalmS != 0 || c.Watch.PostKillS != 0 || c.Watch.SpacingS != 0 {
		t.Errorf("waits of 0: %+v, %v; want them taken", c.Watch, err)
	}
	for _, body := range []string{"watch = 1", "[watch]\nmode = \"kill\"", "[watch]\nsample_log = 1",
		"[watch]\nred_points = \"high\"", "[watch]\ncalm_s = -1", "[watch]\nfloor = \"lots\"",
		"[watch]\nyellow_points = 0", "[watch]\nred_points = 0", "[watch]\nred_samples = 0",
		"[watch]\ngate_pages_s = 0", "[watch]\nfloor = \"0\""} {
		var ce *Error
		if _, err := Load(writeConfig(t, body+"\n")); !errors.As(err, &ce) {
			t.Errorf("Load(%q) = %v, want a *config.Error", body, err)
		}
	}
}

// [telemetry] enabled switches the recorder and, with it, the machine samples. Anything that is
// not a table, or an enabled that is not a boolean, reads as off and is no error. Mutation:
// returning a message for a wrong type turns the last two bodies into errors.
func TestTelemetryTable(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"[telemetry]\nenabled = true\n", true},
		{"", false},
		{"[telemetry]\nenabled = false\n", false},
		{"[telemetry]\n", false},
		{"[telemetry]\nenabled = \"yes\"\n", false},
		{"telemetry = 1\n", false},
	} {
		c, err := Load(writeConfig(t, tc.body))
		if err != nil {
			t.Errorf("Load(%q): %v; want no error", tc.body, err)
			continue
		}
		if c.TelemetryEnabled != tc.want || c.Watch.SampleLog != tc.want {
			t.Errorf("Load(%q): TelemetryEnabled %v, SampleLog %v; want both %v", tc.body, c.TelemetryEnabled, c.Watch.SampleLog, tc.want)
		}
	}
}

// sample_log asks for samples only and enabled asks for both. Mutations: sample_log setting
// TelemetryEnabled too turns the first two rows red; enabled ignored by SampleLog turns the last
// red.
func TestSampleLogSamplesOnly(t *testing.T) {
	for _, tc := range []struct {
		body              string
		enabled, sampling bool
	}{
		{"[watch]\nsample_log = true\n", false, true},
		{"[watch]\nsample_log = true\n[telemetry]\nenabled = false\n", false, true},
		{"[watch]\nsample_log = false\n[telemetry]\nenabled = true\n", true, true},
		{"[watch]\nsample_log = true\n[telemetry]\nenabled = true\n", true, true},
	} {
		c, err := Load(writeConfig(t, tc.body))
		if err != nil {
			t.Fatalf("Load(%q): %v", tc.body, err)
		}
		if c.TelemetryEnabled != tc.enabled || c.Watch.SampleLog != tc.sampling {
			t.Errorf("Load(%q): TelemetryEnabled %v, SampleLog %v; want %v, %v", tc.body, c.TelemetryEnabled, c.Watch.SampleLog, tc.enabled, tc.sampling)
		}
	}
	if _, err := Load(writeConfig(t, "[watch]\nsample_log = 1\n[telemetry]\nenabled = true\n")); err == nil {
		t.Error("a non-boolean sample_log loaded: want today's error")
	}
}
