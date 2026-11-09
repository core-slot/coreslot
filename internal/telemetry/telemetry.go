//go:build unix

// Package telemetry is the opt-in recorder of what a run waited for: one JSON line per event in
// $S/telemetry/events.jsonl. It only observes: it never enters the admission section, never
// blocks, returns no error and recovers its own panics, so a run behaves as with it off.
package telemetry

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/store"
)

// Fields are the keys an event adds to the common ones of its line.
type Fields = map[string]any

// Clocks is where the recorder reads the boot clocks; a test replaces it.
var Clocks = memsample.Clocks

// Recorder is the recorder of one run. A nil *Recorder is valid and writes nothing. It is safe
// for concurrent use: the run's goroutines share it.
type Recorder struct {
	state, runID, label string

	mu      sync.Mutex
	n       int  // lines of the run so far
	lost    int  // lines not written, or panics recovered, since the last line written
	attempt int  // the run's attempt, from 1
	stopped bool // nothing is written any more

	memWG sync.WaitGroup // the machine-wide lines' goroutines in flight; tests wait on it
}

// New returns the recorder of a run. It creates nothing: the directory and the file are made by
// the first line.
func New(stateDir, runID, label string) *Recorder {
	return &Recorder{state: stateDir, runID: runID, label: label, attempt: 1}
}

// SetAttempt sets the attempt the next lines name.
func (r *Recorder) SetAttempt(n int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.attempt = n
	r.mu.Unlock()
}

// Emit writes one line of event e: the common keys of a run's line and f. It returns nothing; a
// line that cannot be written is counted in the lost of the next one.
func (r *Recorder) Emit(event string, f Fields) {
	if r != nil {
		r.emit(event, f, true)
	}
}

// EmitMachine writes a line that names no run: the version, the event, the clocks, lost and f.
func (r *Recorder) EmitMachine(event string, f Fields) {
	if r != nil {
		r.emit(event, f, false)
	}
}

// Stop ends the recorder for a reason FORMAT cannot show: no line is written after it returns.
func (r *Recorder) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
}

// Stopped reports that no line is written any more; a nil recorder is stopped.
func (r *Recorder) Stopped() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.live()
}

// live reports whether a line may be written, with r.mu held. It reads FORMAT each time, so that
// no caller has to: a client never writes into a directory whose format it does not understand, and
// one that finds such a FORMAT, or none, stays silent even if a good one comes back.
func (r *Recorder) live() bool {
	r.stopped = r.stopped || !store.MayWrite(r.state)
	return !r.stopped
}

func (r *Recorder) emit(event string, f Fields, run bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() {
		if recover() != nil {
			r.lost++
		}
	}()
	if !r.live() {
		return
	}
	m := make(map[string]any, len(f)+10)
	for k, v := range f {
		m[k] = v
	}
	up, awake := Clocks()
	m["v"], m["e"], m["time_ns"], m["up_ns"], m["awake_ns"], m["lost"] = store.TelemetrySchema, event, time.Now().UnixNano(), up, awake, r.lost
	if run {
		r.n++
		m["run"], m["attempt"], m["n"], m["label"] = r.runID, r.attempt, r.n, r.label
	}
	b, err := store.EncodeJSON(m)
	if err != nil || len(b)+1 > store.MaxRunLine {
		r.lost++
		return
	}
	if store.AppendFile(filepath.Join(r.state, store.TelemetryDir), store.EventsFile, append(b, '\n')) != nil {
		r.lost++
		return
	}
	r.lost = 0
}
