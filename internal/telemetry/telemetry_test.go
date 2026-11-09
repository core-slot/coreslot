//go:build unix

package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/core-slot/coreslot/internal/store"
)

// stateDir is an absolute state directory for a recorder; the recorder creates telemetry/ itself.
func stateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ana", "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	setFormat(t, dir, fmt.Sprintf("coreslot-protocol %d\n", store.Format))
	return dir
}

func setFormat(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, store.FormatFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func eventsPath(dir string) string { return filepath.Join(dir, store.TelemetryDir, store.EventsFile) }

// lines decodes every line of events.jsonl, failing on one that is not whole JSON.
func lines(t *testing.T, dir string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(eventsPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range bytes.SplitAfter(data, []byte("\n")) {
		if len(l) == 0 {
			continue
		}
		if l[len(l)-1] != '\n' {
			t.Fatalf("a line without its LF: %q", l)
		}
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

// Every line of a run carries the common keys, and n counts the run's lines. Mutation: leaving
// out any of the keys, or numbering from the recorder instead of the run, fails a check here.
func TestEventLineFields(t *testing.T) {
	dir := stateDir(t)
	r := New(dir, "run-1", "go test")
	r.Emit("enqueue", Fields{"ticket": "t1"})
	r.SetAttempt(2)
	r.Emit("admitted", nil)
	got := lines(t, dir)
	if len(got) != 2 {
		t.Fatalf("%d lines, want 2", len(got))
	}
	for i, m := range got {
		for _, k := range []string{"v", "e", "time_ns", "up_ns", "awake_ns", "lost", "run", "attempt", "n", "label"} {
			if _, ok := m[k]; !ok {
				t.Errorf("line %d lacks %q: %v", i+1, k, m)
			}
		}
		if m["v"] != float64(store.TelemetrySchema) || m["run"] != "run-1" || m["label"] != "go test" || m["lost"] != float64(0) {
			t.Errorf("line %d: %v", i+1, m)
		}
	}
	if got[0]["e"] != "enqueue" || got[0]["n"] != float64(1) || got[0]["attempt"] != float64(1) || got[0]["ticket"] != "t1" {
		t.Errorf("first line %v; want enqueue, n 1, attempt 1 and its field", got[0])
	}
	if got[1]["e"] != "admitted" || got[1]["n"] != float64(2) || got[1]["attempt"] != float64(2) {
		t.Errorf("second line %v; want admitted, n 2, attempt 2", got[1])
	}
}

// A machine-wide line is no run's: it has the clocks and lost, and no run, attempt, n or label,
// and it takes no number from the run. Mutation: EmitMachine as Emit adds the run's keys.
func TestEmitMachineNamesNoRun(t *testing.T) {
	dir := stateDir(t)
	r := New(dir, "run-1", "go test")
	r.EmitMachine("memory", Fields{"free_mib": 12})
	r.Emit("enqueue", nil)
	got := lines(t, dir)
	m := got[0]
	for _, k := range []string{"run", "attempt", "n", "label"} {
		if _, ok := m[k]; ok {
			t.Errorf("machine line has %q: %v", k, m)
		}
	}
	for _, k := range []string{"v", "e", "time_ns", "up_ns", "awake_ns", "lost", "free_mib"} {
		if _, ok := m[k]; !ok {
			t.Errorf("machine line lacks %q: %v", k, m)
		}
	}
	if got[1]["n"] != float64(1) {
		t.Errorf("the run's first line has n %v after a machine line, want 1", got[1]["n"])
	}
}

// Nothing is written into a directory whose FORMAT this client does not understand, and a good one
// coming back does not bring the recorder back; the stamps of the machine-wide lines stay unmade.
// Mutations: live not reading FORMAT; a read that does not latch.
func TestFormatSilences(t *testing.T) {
	for name, body := range map[string]string{"foreign": "coreslot-protocol 99\n", "malformed": "coreslot-protocol x\n", "missing": ""} {
		t.Run(name, func(t *testing.T) {
			dir := stateDir(t)
			good, err := os.ReadFile(filepath.Join(dir, store.FormatFile))
			if err != nil {
				t.Fatal(err)
			}
			r := New(dir, "run-1", "x")
			r.Emit("before", nil)
			if body == "" {
				err = os.Remove(filepath.Join(dir, store.FormatFile))
			} else {
				setFormat(t, dir, body)
			}
			if err != nil {
				t.Fatal(err)
			}
			r.MemoryAsync()
			if !r.Stopped() {
				t.Error("the recorder is live")
			}
			setFormat(t, dir, string(good))
			r.Emit("after", nil)
			if got := lines(t, dir); len(got) != 1 || got[0]["e"] != "before" || !r.Stopped() {
				t.Errorf("lines %v, stopped %v; want the first line alone", got, r.Stopped())
			}
			if _, err := os.Stat(filepath.Join(dir, store.TelemetryDir, store.MemoryStamp)); err == nil {
				t.Error("a stamp was made")
			}
		})
	}
}

func TestNilWritesNothing(t *testing.T) {
	var r *Recorder
	r.SetAttempt(2)
	r.Emit("enqueue", Fields{"a": 1})
	r.EmitMachine("memory", nil)
	r.Stop()
}

// Writes into a directory that cannot be written return nothing and are counted; the next line
// that can be written carries the count. Mutation: resetting lost when a write fails reads 0.
func TestUnwritableDirectoryCountsLost(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := stateDir(t)
	tdir := filepath.Join(dir, store.TelemetryDir)
	if err := os.Mkdir(tdir, 0o500); err != nil {
		t.Fatal(err)
	}
	r := New(dir, "run-1", "x")
	for range 3 {
		r.Emit("waiting", nil)
	}
	if err := os.Chmod(tdir, 0o700); err != nil {
		t.Fatal(err)
	}
	r.Emit("admitted", nil)
	r.Emit("ended", nil)
	got := lines(t, dir)
	if len(got) != 2 || got[0]["lost"] != float64(3) || got[1]["lost"] != float64(0) {
		t.Errorf("lines %v; want two, the first with lost 3 and the second with lost 0", got)
	}
}

// A line over the run log's limit is dropped and counted. Mutation: writing it anyway.
func TestEmitOverLimitCountsLost(t *testing.T) {
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	r.Emit("enqueue", Fields{"big": strings.Repeat("a", store.MaxRunLine)})
	r.Emit("admitted", nil)
	got := lines(t, dir)
	if len(got) != 1 || got[0]["e"] != "admitted" || got[0]["lost"] != float64(1) {
		t.Errorf("lines %v; want only admitted, with lost 1", got)
	}
}

type boom struct{}

func (boom) MarshalJSON() ([]byte, error) { panic("boom") }

// A panic while a line is built is recovered and counted, and the recorder goes on. Mutation:
// dropping the recover in emit lets the panic out; the clocks seam is the other place that can.
func TestEmitPanicRecovered(t *testing.T) {
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	r.Emit("enqueue", Fields{"x": boom{}})
	saved := Clocks
	Clocks = func() (int64, int64) { panic("clocks") }
	r.Emit("waiting", nil)
	r.EmitMachine("memory", nil)
	Clocks = saved
	r.Emit("admitted", nil)
	got := lines(t, dir)
	if len(got) != 1 || got[0]["e"] != "admitted" || got[0]["lost"] != float64(3) {
		t.Errorf("lines %v; want only admitted, with lost 3", got)
	}
}

// Nothing is written through a symlinked events file, nor into a symlinked telemetry directory.
// Mutation: opening without O_NOFOLLOW, or not checking the directory, writes through.
func TestSymlinkRefused(t *testing.T) {
	dir := stateDir(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, store.TelemetryDir)); err != nil {
		t.Fatal(err)
	}
	r := New(dir, "run-1", "x")
	r.Emit("enqueue", nil)
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Errorf("a symlinked telemetry/ was written through: %v", ents)
	}

	dir = stateDir(t)
	target := filepath.Join(t.TempDir(), "target.jsonl")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, store.TelemetryDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, eventsPath(dir)); err != nil {
		t.Fatal(err)
	}
	New(dir, "run-1", "x").Emit("enqueue", nil)
	if b, _ := os.ReadFile(target); len(b) != 0 {
		t.Errorf("a symlinked events.jsonl was written through: %q", b)
	}
}

// Nothing is written after Stop, and nothing is created before the first Emit. Mutation: Stop
// that sets nothing.
func TestStopSilences(t *testing.T) {
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	if _, err := os.Lstat(filepath.Join(dir, store.TelemetryDir)); err == nil {
		t.Fatal("New created telemetry/")
	}
	r.Emit("enqueue", nil)
	r.Stop()
	r.Emit("admitted", nil)
	r.EmitMachine("memory", nil)
	if got := lines(t, dir); len(got) != 1 {
		t.Errorf("%d lines after Stop, want the 1 before it", len(got))
	}
}

// Eight goroutines share a recorder: every line is whole and n is unique. Mutation: numbering
// outside the mutex gives two lines one n, or a data race under -race.
func TestConcurrentEmit(t *testing.T) {
	dir := stateDir(t)
	r := New(dir, "run-1", "x")
	const workers, each = 8, 50
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				r.Emit("waiting", Fields{"pad": strings.Repeat("p", 200)})
			}
		}()
	}
	wg.Wait()
	seen := map[float64]bool{}
	for _, m := range lines(t, dir) {
		n := m["n"].(float64)
		if seen[n] {
			t.Errorf("n %v twice", n)
		}
		seen[n] = true
	}
	if len(seen) != workers*each {
		t.Errorf("%d distinct lines, want %d", len(seen), workers*each)
	}
}

// The recorder never enters the admission section on its own. Mutation: a call to Enter or
// TryEnter anywhere in the package.
func TestEntersNoSection(t *testing.T) {
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Enter" || sel.Sel.Name == "TryEnter") {
				t.Errorf("%s: enters the section", fset.Position(sel.Pos()))
			}
			return true
		})
	}
}
