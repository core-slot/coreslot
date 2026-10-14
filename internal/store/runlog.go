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

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/wire"
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
	Nested              bool // a pass-through run (PROTOCOL.md section 18); encoded only when true
	// CPUUserS, CPUSysS and MaxRSSKiB are the command's user and system CPU seconds and its peak
	// resident set size in KiB, from its wait4 rusage (PROTOCOL.md section 20). Each is nil when
	// it is not known (the command never started, or the platform does not report it) and is
	// then not encoded, so that an unknown value is never written as a measured zero.
	CPUUserS  *float64
	CPUSysS   *float64
	MaxRSSKiB *int64
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
	CPUUserS            *float64          `json:"cpu_user_s,omitempty"`
	CPUSysS             *float64          `json:"cpu_sys_s,omitempty"`
	MaxRSSKiB           *int64            `json:"max_rss_kib,omitempty"`
	Exit                int               `json:"exit"`
	Load1               float64           `json:"load1"`
	FailOpen            string            `json:"fail_open"`
	Nested              bool              `json:"nested,omitempty"`
}

func (r RunRecord) wire() runJSON {
	need := make(map[string][2]int, len(r.Need))
	for name, rg := range r.Need {
		need[name] = [2]int{rg.Min, rg.Max}
	}
	j := runJSON{TimeNS: r.TimeNS, Label: r.Label, Need: need, Grant: r.Grant,
		Clamped: r.Clamped, UnbudgetedResources: r.UnbudgetedResources, WaitedS: r.WaitedS,
		RanS: r.RanS, CPUUserS: r.CPUUserS, CPUSysS: r.CPUSysS, MaxRSSKiB: r.MaxRSSKiB, Exit: r.Exit,
		Load1: r.Load1, FailOpen: r.FailOpen, Nested: r.Nested}
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
	return wire.EscapeJSON(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// AppendRun appends r to log/runs.jsonl with one O_APPEND write of at most 4 KiB, outside the
// section. It recreates log/ when a reset removed it. It never rotates, so a pass-through run
// (a record with Nested set, PROTOCOL.md sections 18 and 20) can call it without entering the
// section. Rotation is separate: a caller that finds RunLogNeedsRotation rotates first with
// RotateLog.
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
	n, err := writeOnce(f, line)
	if err == nil && n != len(line) {
		err = fmt.Errorf("store: short run-log write: %d of %d bytes", n, len(line))
	}
	return err
}

// writeOnce issues exactly one write(2) of b on f (repeated only when it was interrupted before
// writing anything), so an O_APPEND line is never split across two writes. os.File.Write would
// loop on a short write and append the rest separately.
func writeOnce(f *os.File, b []byte) (int, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int
	var werr error
	if err := rc.Control(func(fd uintptr) {
		for {
			n, werr = unix.Write(int(fd), b)
			if !errors.Is(werr, unix.EINTR) {
				return
			}
		}
	}); err != nil {
		return 0, err
	}
	if werr != nil {
		return 0, &os.PathError{Op: "write", Path: f.Name(), Err: werr}
	}
	return n, nil
}

// RunLogNeedsRotation reports whether log/runs.jsonl is at or above 10 MiB.
func (s *Store) RunLogNeedsRotation() bool {
	fi, err := os.Lstat(s.path(LogDir, RunLog))
	return err == nil && fi.Size() >= RotateRunLogAt
}

// RotateLog rotates the run log (PROTOCOL.md section 20) for a caller that is not inside the
// section: it enters the section (giving up after 2 s), reads FORMAT, refuses when the format is
// not understood, reconciles if due (section 10.4), and renames log/runs.jsonl to runs.1.jsonl
// when it is still at or above 10 MiB. A section it cannot enter is a *NotEnteredError, and a
// FORMAT that is not understood, malformed or unreadable a *FormatError; it rotates nothing
// then. It must not be called while this process holds the section: entering again would fail.
func (s *Store) RotateLog() error {
	return s.inSideSection(func(sec *Section) error { return sec.rotateLogIfNeeded() })
}

// rotateLogIfNeeded rotates the run log inside the section: when log/runs.jsonl is still at or
// above 10 MiB it renames it to runs.1.jsonl, replacing the previous generation.
func (sec *Section) rotateLogIfNeeded() error {
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
