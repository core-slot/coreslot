//go:build unix

package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/wire"
)

// Run-log limits (PROTOCOL.md section 20).
const (
	MaxRunLine     = 4096     // one line, LF included
	RotateRunLogAt = 10 << 20 // 10 MiB
)

// RunRecord is one run-log line, its keys in the order of the fields. It has no field for argv or
// the environment, which the log never contains.
type RunRecord struct {
	TimeNS  int64   `json:"time_ns"`
	Label   string  `json:"label"`
	WaitedS float64 `json:"waited_s"`
	RanS    float64 `json:"ran_s"`
	// UserS and SysS are the command's user and system seconds from its wait4 rusage (PROTOCOL.md
	// section 20). Each is nil when it is not known (the command never started, or the platform
	// does not report it) and is then not encoded, so that an unknown value is never written as a
	// measured zero.
	UserS *float64 `json:"user_s,omitempty"`
	SysS  *float64 `json:"sys_s,omitempty"`
	// MemPeakKiB is the command tree's memory peak in KiB and MemPeakSource its measure: the
	// sampler's (memsample.Source) or "max_rss"; PeakAfterS is when the sampler read it, in
	// seconds after the start. WaitedFreeS is how long the run waited while the memory it needs
	// was free. Each is nil or empty when unknown, and is then not encoded.
	MemPeakKiB    *int64   `json:"mem_peak_kib,omitempty"`
	MemPeakSource string   `json:"mem_peak_source,omitempty"`
	PeakAfterS    *float64 `json:"peak_after_s,omitempty"`
	WaitedFreeS   *float64 `json:"waited_free_s,omitempty"`
	Stops         int      `json:"stops,omitempty"` // times the run was stopped; encoded only above 0
	Exit          int      `json:"exit"`
	FailOpen      string   `json:"fail_open"`
	Nested        bool     `json:"nested,omitempty"` // a pass-through run (PROTOCOL.md section 18)
}

// ReadRuns reads the run log, the rotated generation (log/runs.1.jsonl) first and then
// log/runs.jsonl, each through OpenFile; a missing file is empty. A line that does not decode is
// skipped and counted in skipped. A key a line lacks is left unknown.
func (s *Store) ReadRuns() (runs []RunRecord, skipped int, err error) {
	for _, name := range []string{RunLogOld, RunLog} {
		f, err := OpenFile(s.path(LogDir, name), os.O_RDONLY)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, 0, err
		}
		for line := range bytes.Lines(data) {
			var r RunRecord
			if json.Unmarshal(line, &r) != nil {
				skipped++
				continue
			}
			runs = append(runs, r)
		}
	}
	return runs, skipped, nil
}

// line encodes r as one JSON line of at most MaxRunLine bytes, LF included, shortening the
// fail-open reason when that alone makes it fit.
func (r RunRecord) line() ([]byte, error) {
	for {
		b, err := encodeJSON(r)
		if err != nil {
			return nil, err
		}
		if len(b)+1 <= MaxRunLine {
			return append(b, '\n'), nil
		}
		if r.FailOpen == "" {
			return nil, fmt.Errorf("store: run record is %d bytes, more than %d", len(b)+1, MaxRunLine)
		}
		// Drop at least the excess, on a rune boundary; escaping can make the next pass shorter
		// than needed, so loop until it fits.
		cut := max(len(r.FailOpen)-(len(b)+1-MaxRunLine), 0)
		for cut > 0 && !utf8.RuneStart(r.FailOpen[cut]) {
			cut--
		}
		r.FailOpen = r.FailOpen[:cut]
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
	return s.appendLine(RunLog, line)
}

// appendLine appends line to log/name with one write, recreating log/ when a reset removed it.
func (s *Store) appendLine(name string, line []byte) error {
	if err := ensureDir(s.path(LogDir)); err != nil {
		return err
	}
	f, err := OpenFile(s.path(LogDir, name), os.O_WRONLY|os.O_APPEND|os.O_CREATE)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := writeOnce(f, line)
	if err == nil && n != len(line) {
		err = fmt.Errorf("store: short log write: %d of %d bytes", n, len(line))
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
