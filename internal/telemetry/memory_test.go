//go:build unix

package telemetry

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/store"
)

// fakeSweep replaces the sweep seam with one that counts its calls.
func fakeSweep(t *testing.T, fn func() ([]memsample.ProcMem, *memsample.ProcMem, bool)) *int {
	t.Helper()
	calls := 0
	saved := Sweep
	Sweep = func(n int) ([]memsample.ProcMem, *memsample.ProcMem, bool) { calls++; return fn() }
	t.Cleanup(func() { Sweep = saved })
	return &calls
}

func goodSweep() ([]memsample.ProcMem, *memsample.ProcMem, bool) {
	vm := &memsample.ProcMem{Name: "com.docker.virtualization", FootprintMiB: 6000, ResidentMiB: 5000, CPUS: 12.5, StartNS: 77}
	return []memsample.ProcMem{*vm, {Name: "Safari", FootprintMiB: 900, ResidentMiB: 800}}, vm, true
}

func stampPath(dir string) string { return filepath.Join(dir, store.TelemetryDir, store.MemoryStamp) }

// The line names no run, carries the VM and the top, and a second call inside the window writes
// nothing. Mutations: not touching the stamp writes two lines; writing it with Emit gives it a run.
func TestMemoryOncePerWindow(t *testing.T) {
	calls := fakeSweep(t, goodSweep)
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	r.MemoryAsync()
	r.memWG.Wait()
	r.MemoryAsync()
	r.memWG.Wait()
	got := lines(t, dir)
	if len(got) != 1 || *calls != 1 {
		t.Fatalf("%d lines and %d sweeps, want 1 and 1: %v", len(got), *calls, got)
	}
	l := got[0]
	if l["e"] != "memory" || l["run"] != nil || l["n"] != nil {
		t.Errorf("the line names a run or is not memory: %v", l)
	}
	vm, _ := l["vm"].(map[string]any)
	if vm["name"] != "com.docker.virtualization" || vm["footprint_mib"] != float64(6000) || vm["resident_mib"] != float64(5000) ||
		vm["cpu_s"] != 12.5 || vm["start_ns"] != float64(77) {
		t.Errorf("vm = %v", vm)
	}
	top, _ := l["top"].([]any)
	if len(top) != 2 || top[1].(map[string]any)["name"] != "Safari" || top[1].(map[string]any)["footprint_mib"] != float64(900) {
		t.Errorf("top = %v", top)
	}
	if _, has := top[0].(map[string]any)["resident_mib"]; has {
		t.Errorf("a top entry carries resident_mib: %v", top[0])
	}
}

// A stamp whose time is in the future (a clock that went back) is due, and so is one past the
// window, which ends before a run's 30 s cadence, so a lone run's next ask finds it due; one inside
// the window is not. Mutations: dropping the future test leaves the first row silent; a window of
// 30 s silences the "past" row; one of 20 s sweeps in the "inside" row.
func TestFutureStampIsDue(t *testing.T) {
	for _, c := range []struct {
		name string
		age  time.Duration
		due  bool
	}{{"future", -time.Hour, true}, {"old", 31 * time.Second, true}, {"past the window", 27 * time.Second, true}, {"inside the window", 23 * time.Second, false}, {"fresh", 10 * time.Second, false}} {
		t.Run(c.name, func(t *testing.T) {
			calls := fakeSweep(t, goodSweep)
			dir := stateDir(t)
			if err := os.MkdirAll(filepath.Dir(stampPath(dir)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stampPath(dir), []byte("\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			at := time.Now().Add(-c.age)
			if err := os.Chtimes(stampPath(dir), at, at); err != nil {
				t.Fatal(err)
			}
			r := New(dir, "run-1", "x")
			r.MemoryAsync()
			r.memWG.Wait()
			if got := *calls == 1; got != c.due {
				t.Errorf("swept %v, want %v", got, c.due)
			}
		})
	}
}

// A panic in the sweep is recovered and counted in the next line's lost, and the caller never sees
// it. Mutation: dropping the recover in the goroutine kills the test binary.
func TestMemoryPanicRecovered(t *testing.T) {
	fakeSweep(t, func() ([]memsample.ProcMem, *memsample.ProcMem, bool) { panic("sweep") })
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	r.MemoryAsync()
	r.memWG.Wait()
	r.Emit("admitted", nil)
	if got := lines(t, dir); len(got) != 1 || got[0]["lost"] != float64(1) {
		t.Errorf("lines %v; want admitted with lost 1", got)
	}
}

// The caller does not wait for the sweep, and the line comes when it ends. Mutation: running the
// sweep in the caller takes the full second.
func TestMemoryNeverBlocksCaller(t *testing.T) {
	fakeSweep(t, func() ([]memsample.ProcMem, *memsample.ProcMem, bool) { time.Sleep(time.Second); return goodSweep() })
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	start := time.Now()
	r.MemoryAsync()
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("MemoryAsync took %v", d)
	}
	r.memWG.Wait()
	if got := lines(t, dir); len(got) != 1 {
		t.Errorf("%d lines after the sweep, want 1", len(got))
	}
}

// A sweep that reads nothing writes no line, a nil or stopped recorder sweeps nothing, and an
// unwritable directory costs a line and no more. Mutation: writing the line when ok is false.
func TestMemoryWritesOnlyWhatItRead(t *testing.T) {
	calls := fakeSweep(t, func() ([]memsample.ProcMem, *memsample.ProcMem, bool) { return nil, nil, false })
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	r.MemoryAsync()
	r.memWG.Wait()
	if _, err := os.Stat(eventsPath(dir)); err == nil {
		t.Error("a line was written for a sweep that read nothing")
	}
	var nilRec *Recorder
	nilRec.MemoryAsync()
	stopped := New(stateDir(t), "run-2", "x")
	stopped.Stop()
	stopped.MemoryAsync()
	stopped.memWG.Wait()
	if *calls != 1 {
		t.Errorf("%d sweeps, want 1 (nil and stopped recorders sweep nothing)", *calls)
	}
}
