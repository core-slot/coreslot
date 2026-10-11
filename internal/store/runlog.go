//go:build unix

package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unicode/utf8"

	"github.com/Nezhinskiy/coreslot/internal/spec"
)

// Run-log limits (PROTOCOL.md section 20).
const (
	MaxRunLine     = 4096     // one line, LF included
	RotateRunLogAt = 10 << 20 // 10 MiB
)

// RunRecord is one run-log line. It has no field for argv or the environment, which the log
// never contains.
type RunRecord struct {
	TimeNS              int64
	Label               string
	Need                map[string]spec.Range
	Grant               map[string]int
	Clamped             []string
	UnbudgetedResources []string
	WaitedS             float64
	RanS                float64
	Exit                int
	Load1               float64
	FailOpen            string
}

type runJSON struct {
	TimeNS              int64             `json:"time_ns"`
	Label               string            `json:"label"`
	Need                map[string][2]int `json:"need"`
	Grant               map[string]int    `json:"grant"`
	Clamped             []string          `json:"clamped"`
	UnbudgetedResources []string          `json:"unbudgeted_resources"`
	WaitedS             float64           `json:"waited_s"`
	RanS                float64           `json:"ran_s"`
	Exit                int               `json:"exit"`
	Load1               float64           `json:"load1"`
	FailOpen            string            `json:"fail_open"`
}

func (r RunRecord) wire() runJSON {
	need := make(map[string][2]int, len(r.Need))
	for name, rg := range r.Need {
		need[name] = [2]int{rg.Min, rg.Max}
	}
	j := runJSON{TimeNS: r.TimeNS, Label: r.Label, Need: need, Grant: r.Grant,
		Clamped: r.Clamped, UnbudgetedResources: r.UnbudgetedResources, WaitedS: r.WaitedS,
		RanS: r.RanS, Exit: r.Exit, Load1: r.Load1, FailOpen: r.FailOpen}
	if j.Grant == nil {
		j.Grant = map[string]int{}
	}
	if j.Clamped == nil {
		j.Clamped = []string{}
	}
	if j.UnbudgetedResources == nil {
		j.UnbudgetedResources = []string{}
	}
	return j
}

// line encodes r as one JSON line of at most MaxRunLine bytes, LF included, shortening the
// fail-open reason when that alone makes it fit.
func (r RunRecord) line() ([]byte, error) {
	j := r.wire()
	for {
		b, err := encodeJSON(j)
		if err != nil {
			return nil, err
		}
		if len(b)+1 <= MaxRunLine {
			return append(b, '\n'), nil
		}
		if j.FailOpen == "" {
			return nil, fmt.Errorf("store: run record is %d bytes, more than %d", len(b)+1, MaxRunLine)
		}
		// Drop at least the excess, on a rune boundary; escaping can make the next pass shorter
		// than needed, so loop until it fits.
		cut := max(len(j.FailOpen)-(len(b)+1-MaxRunLine), 0)
		for cut > 0 && !utf8.RuneStart(j.FailOpen[cut]) {
			cut--
		}
		j.FailOpen = j.FailOpen[:cut]
	}
}

func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func jsonString(s string) ([]byte, error) { return encodeJSON(s) }

// AppendRun appends r to log/runs.jsonl with one O_APPEND write of at most 4 KiB, outside the
// section. It recreates log/ when a reset removed it. Rotation is separate: a caller that finds
// RunLogNeedsRotation rotates inside the section first.
func (s *Store) AppendRun(r RunRecord) error {
	line, err := r.line()
	if err != nil {
		return err
	}
	if err := ensureDir(s.path(LogDir)); err != nil {
		return err
	}
	f, err := OpenFile(s.path(LogDir, RunLog), os.O_WRONLY|os.O_APPEND|os.O_CREATE)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := f.Write(line)
	if err == nil && n != len(line) {
		err = fmt.Errorf("store: short run-log write: %d of %d bytes", n, len(line))
	}
	return err
}

// RunLogNeedsRotation reports whether log/runs.jsonl is at or above 10 MiB.
func (s *Store) RunLogNeedsRotation() bool {
	fi, err := os.Lstat(s.path(LogDir, RunLog))
	return err == nil && fi.Size() >= RotateRunLogAt
}

// RotateLogIfNeeded rotates the run log inside the section: when log/runs.jsonl is still at or
// above 10 MiB it renames it to runs.1.jsonl, replacing the previous generation.
func (sec *Section) RotateLogIfNeeded() error {
	if err := sec.live(); err != nil {
		return err
	}
	p := sec.s.path(LogDir, RunLog)
	if err := CheckEntry(sec.s.path(LogDir), true); err != nil {
		return err
	}
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkInfo(p, fi, false); err != nil {
		return err
	}
	if fi.Size() < RotateRunLogAt {
		return nil
	}
	old := sec.s.path(LogDir, RunLogOld)
	if err := CheckEntry(old, false); err != nil {
		return err
	}
	return os.Rename(p, old)
}
