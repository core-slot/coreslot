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
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/testhooks"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// seq is this process's publication counter (PROTOCOL.md section 7.1), shared by every Store
// in the process so that two publications of one process never share a name.
var seq atomic.Uint64

// NextSeq returns this process's next seq: 1, 2, ... 999999, 0, 1, ...
func NextSeq() int {
	return int(seq.Add(1) % wire.SeqModulus)
}

// tmpName is a temporary file's name, ".tmp-" pid "-" seq (section 7.1).
func tmpName(pid, seq int) string {
	return fmt.Sprintf(".tmp-%010d-%06d", pid, seq)
}

// isTmpName reports whether name matches the temporary-file grammar exactly.
func isTmpName(name string) bool {
	const prefix = ".tmp-"
	const dash = len(prefix) + 10
	if len(name) != dash+1+6 || name[:len(prefix)] != prefix || name[dash] != '-' {
		return false
	}
	for i := len(prefix); i < len(name); i++ {
		if i != dash && (name[i] < '0' || name[i] > '9') {
			return false
		}
	}
	return true
}

// NowNS is the wall clock in nanoseconds, shifted by the test hook's offset in test builds.
func NowNS() int64 {
	return time.Now().Add(testhooks.RealtimeOffset()).UnixNano()
}

// point is the pause-point hook; tests replace it to observe publication from inside.
var point = testhooks.Point

// renameNoReplace moves from to to, failing with EEXIST when to exists; tests replace it to
// force the link+unlink fallback.
var renameNoReplace = renameNoReplaceOS

// maxTmpAttempts bounds the search for a free temporary name.
const maxTmpAttempts = 1000

// noReplaceDirs are the directories whose entries are published without replacement.
var noReplaceDirs = []string{QueueDir, HoldsDir, CancelDir}

// Publish publishes body as dir/name inside the section (PROTOCOL.md section 11), where dir is
// QueueDir, HoldsDir or CancelDir: it creates dir/.tmp-<pid>-<seq> with O_EXCL (taking the next
// seq on EEXIST), takes flock(LOCK_EX) on it when lockCarrying, writes the body, and moves it to
// name without replacing an existing file. When name exists it removes its temporary file and
// returns an error matching fs.ErrExist: the caller takes its next seq and retries.
//
// For a lock-carrying file it returns the locked descriptor, which the caller keeps open for as
// long as the entry is live; otherwise it returns nil. A directory already holding MaxEntries
// entries is a *StateError.
func (sec *Section) Publish(dir, name string, body []byte, lockCarrying bool) (*os.File, error) {
	if err := sec.live(); err != nil {
		return nil, err
	}
	if !slices.Contains(noReplaceDirs, dir) {
		return nil, fmt.Errorf("store: Publish into %q: not a directory of tickets, holds or cancel requests", dir)
	}
	if name == "" || name != filepath.Base(name) || name[0] == '.' {
		return nil, fmt.Errorf("store: Publish of %q: not an entry name", name)
	}
	d := sec.s.path(dir)
	if err := ensureDir(d); err != nil {
		return nil, err
	}
	existing, err := readNames(d)
	if err != nil {
		return nil, err
	}
	if len(existing) >= maxEntries {
		return nil, stateErrorf("%s is full: it holds %d entries", d, len(existing))
	}
	f, tmp, err := sec.writeTmp(d, body, lockCarrying)
	if err != nil {
		return nil, err
	}
	point("publish.before-rename")
	final := filepath.Join(d, name)
	if err := moveNoReplace(tmp, final); err != nil {
		os.Remove(tmp)
		f.Close()
		return nil, err
	}
	if !lockCarrying {
		f.Close()
		return nil, nil
	}
	return f, nil
}

// writeTmp creates a fresh temporary file in d, locks it when lockCarrying, and writes body,
// passing the publish.after-create, publish.after-lock and publish.after-write points.
func (sec *Section) writeTmp(d string, body []byte, lockCarrying bool) (*os.File, string, error) {
	pid := os.Getpid()
	var f *os.File
	var tmp string
	for attempt := 0; ; attempt++ {
		if attempt == maxTmpAttempts {
			return nil, "", fmt.Errorf("store: no free temporary name in %s after %d attempts", d, attempt)
		}
		tmp = filepath.Join(d, tmpName(pid, NextSeq()))
		var err error
		f, err = OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL)
		if errors.Is(err, fs.ErrExist) {
			continue // a young leftover of an earlier process with this pid
		}
		if err != nil {
			return nil, "", err
		}
		break
	}
	fail := func(err error) (*os.File, string, error) {
		os.Remove(tmp)
		f.Close()
		return nil, "", err
	}
	point("publish.after-create")
	if lockCarrying {
		ok, err := flockNB(f)
		if err != nil {
			return fail(err)
		}
		if !ok {
			return fail(fmt.Errorf("store: the fresh temporary file %s is locked by another description", tmp))
		}
	}
	point("publish.after-lock")
	if _, err := f.Write(body); err != nil {
		return fail(err)
	}
	point("publish.after-write")
	return f, tmp, nil
}

// moveNoReplace moves tmp to final without replacing final: renameat2(RENAME_NOREPLACE) or
// renamex_np(RENAME_EXCL), and where that is unavailable (EINVAL, ENOSYS, ENOTSUP), link(2) to
// the final name followed by unlink(2) of the temporary name.
func moveNoReplace(tmp, final string) error {
	err := renameNoReplace(tmp, final)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOSYS) &&
		!errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
		return &os.LinkError{Op: "rename", Old: tmp, New: final, Err: err}
	}
	if err := unix.Link(tmp, final); err != nil {
		return &os.LinkError{Op: "link", Old: tmp, New: final, Err: err}
	}
	// The entry is published once linked. A failed unlink leaves a temporary name on the same
	// inode, which reconciliation removes; reporting failure here would orphan a live entry.
	_ = unix.Unlink(tmp)
	return nil
}

// replaceFile publishes body as the top-level file name (gen, FORMAT, degraded) inside the
// section: temporary file in the state directory, then a plain rename over the old file.
func (sec *Section) replaceFile(name string, body []byte) error {
	if err := sec.live(); err != nil {
		return err
	}
	f, tmp, err := sec.writeTmp(sec.s.dir, body, false)
	if err != nil {
		return err
	}
	f.Close()
	point("publish.before-rename")
	if err := os.Rename(tmp, sec.s.path(name)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// readSmall reads a regular file of the layout of at most limit bytes; a missing file returns
// fs.ErrNotExist, a larger one returns its first limit+1 bytes.
func (s *Store) readSmall(name string, limit int) ([]byte, error) {
	f, err := OpenFile(s.path(name), os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(limit)+1))
}

// ---------------------------------------------------------------- gen

// Gen is the content of gen (PROTOCOL.md section 7.8).
type Gen struct {
	N            uint64
	ReconciledNS int64
}

// ReadGen reads gen. A missing or malformed gen reads as the zero Gen; a gen that breaks the
// filesystem policy is a *StateError.
func (s *Store) ReadGen() (Gen, error) {
	body, err := s.readSmall(GenFile, wire.MaxBodyBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return Gen{}, nil
	}
	if err != nil {
		return Gen{}, err
	}
	obj, err := strictObject(body)
	if err != nil {
		return Gen{}, nil
	}
	n, err1 := obj.integer("gen")
	r, err2 := obj.integer("reconciled_ns")
	if err1 != nil || err2 != nil || n < 0 {
		return Gen{}, nil
	}
	return Gen{N: uint64(n), ReconciledNS: r}, nil
}

func (sec *Section) writeGen(g Gen) error {
	return sec.replaceFile(GenFile, fmt.Appendf(nil, `{"gen":%d,"reconciled_ns":%d}`, g.N, g.ReconciledNS))
}

// BumpGen writes gen with gen increased by one and reconciled_ns unchanged.
func (sec *Section) BumpGen() error {
	if err := sec.live(); err != nil {
		return err
	}
	point("gen.before-bump")
	g, err := sec.s.ReadGen()
	if err != nil {
		return err
	}
	g.N++
	return sec.writeGen(g)
}

// ---------------------------------------------------------------- FORMAT

const formatPrefix = "coreslot-protocol "

// Format reads FORMAT, which every entry does before changing anything (PROTOCOL.md section
// 8). When FORMAT is absent it writes `coreslot-protocol 1`. It reports the format n and
// whether it is among implemented. A malformed FORMAT is a *StateError. When the format is
// understood it also rewrites admit.lock's informational body to name this holder; when it is
// not, it writes nothing at all (a token-only client changes nothing but token locks).
func (sec *Section) Format(implemented []int) (n int, understood bool, err error) {
	if err := sec.live(); err != nil {
		return 0, false, err
	}
	body, err := sec.s.readSmall(FormatFile, 64)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := sec.replaceFile(FormatFile, []byte(formatPrefix+"1\n")); err != nil {
			return 0, false, err
		}
		n = 1
	case err != nil:
		return 0, false, err
	default:
		n, err = parseFormat(body)
		if err != nil {
			return 0, false, err
		}
	}
	if !slices.Contains(implemented, n) {
		return n, false, nil
	}
	if err := sec.markHolder(); err != nil {
		return n, true, err
	}
	return n, true, nil
}

func parseFormat(body []byte) (int, error) {
	line := bytes.TrimSuffix(body, []byte("\n"))
	digits, ok := bytes.CutPrefix(line, []byte(formatPrefix))
	if !ok || len(digits) == 0 || len(digits) > 9 {
		return 0, stateErrorf("malformed FORMAT %q", body)
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, stateErrorf("malformed FORMAT %q", body)
		}
	}
	n, err := strconv.Atoi(string(digits))
	if err != nil {
		return 0, stateErrorf("malformed FORMAT %q", body)
	}
	return n, nil
}

// ---------------------------------------------------------------- strict JSON

// object is a decoded body object: values are json.Number, string, bool, nil, object or []any.
type object map[string]any

var errMalformed = errors.New("malformed body")

// strictObject decodes one UTF-8 JSON object of at most 8 KiB, optionally surrounded by
// whitespace, refusing duplicate keys at any depth (PROTOCOL.md section 7.2).
func strictObject(body []byte) (object, error) {
	if len(body) > wire.MaxBodyBytes || !utf8.Valid(body) {
		return nil, errMalformed
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	v, err := readValue(dec)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(object)
	if !ok {
		return nil, errMalformed
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errMalformed
	}
	return obj, nil
}

func readValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, errMalformed
	}
	switch tok {
	case json.Delim('{'):
		obj := object{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return nil, errMalformed
			}
			key, ok := k.(string)
			if _, dup := obj[key]; !ok || dup {
				return nil, errMalformed
			}
			if obj[key], err = readValue(dec); err != nil {
				return nil, err
			}
		}
		if _, err := dec.Token(); err != nil {
			return nil, errMalformed
		}
		return obj, nil
	case json.Delim('['):
		var arr []any
		for dec.More() {
			v, err := readValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, errMalformed
		}
		return arr, nil
	}
	return tok, nil
}

// integer returns key as a signed 64-bit integer with no fraction or exponent.
func (o object) integer(key string) (int64, error) {
	num, ok := o[key].(json.Number)
	if !ok {
		return 0, errMalformed
	}
	n, err := strconv.ParseInt(string(num), 10, 64)
	if err != nil {
		return 0, errMalformed
	}
	return n, nil
}

func (o object) str(key string) (string, error) {
	s, ok := o[key].(string)
	if !ok {
		return "", errMalformed
	}
	return s, nil
}
