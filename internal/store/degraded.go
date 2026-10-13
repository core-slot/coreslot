//go:build unix

package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Nezhinskiy/coreslot/internal/testhooks"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// sideSectionWait bounds how long a Store-level write that needs the section (clearing the
// degraded record) waits to enter it: the same 2 s a run waits to remove its hold after its command
// exits (PROTOCOL.md section 12.3).
const sideSectionWait = 2 * time.Second

// DegradedRecord is the degraded-mode record (PROTOCOL.md section 7.9). Degraded mode is
// recorded while the file exists, whatever its body: Malformed reports a body that could not be
// read, whose other fields are then zero.
type DegradedRecord struct {
	Reason    string
	FirstNS   int64
	Count     int64
	Malformed bool
}

// Degraded reads the degraded record: nil when degraded mode is not recorded.
func (s *Store) Degraded() (*DegradedRecord, error) {
	body, err := s.ReadSmall(DegradedFile, wire.MaxBodyBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := wire.DecodeDegraded(body)
	if err != nil {
		return &DegradedRecord{Malformed: true}, nil
	}
	return &DegradedRecord{Reason: d.Reason, FirstNS: d.FirstNS, Count: d.Count}, nil
}

// RecordDegraded records a fail-open run inside the section: reason becomes the most recent
// reason, the count increases by one, and first_ns is kept, or set to now when there was no
// readable record. The caller has read FORMAT and reconciled if due.
func (sec *Section) RecordDegraded(reason string) error {
	if err := sec.live(); err != nil {
		return err
	}
	rec := DegradedRecord{FirstNS: NowNS()}
	if old, err := sec.s.Degraded(); err != nil {
		return err
	} else if old != nil && !old.Malformed {
		rec = *old
	}
	rec.Reason = reason
	rec.Count++
	body, err := encodeDegraded(rec)
	if err != nil {
		return err
	}
	return sec.replaceFile(DegradedFile, body)
}

// ClearDegraded removes the degraded record inside the section; no record is not an error.
func (sec *Section) ClearDegraded() error {
	if err := sec.live(); err != nil {
		return err
	}
	if err := os.Remove(sec.s.path(DegradedFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// encodeDegraded writes the record's body, shortening the reason (on a rune boundary) until the
// body fits the 8 KiB every body must fit.
func encodeDegraded(r DegradedRecord) ([]byte, error) {
	r.Reason = strings.ToValidUTF8(r.Reason, "\uFFFD")
	for {
		body, err := wire.EncodeDegraded(wire.Degraded{Reason: r.Reason, FirstNS: r.FirstNS, Count: r.Count})
		if err == nil {
			return body, nil
		}
		raw, jerr := encodeJSON(r.Reason)
		if jerr != nil || r.Reason == "" {
			return nil, err
		}
		// Drop at least the excess of the escaped reason, on a rune boundary, and try again.
		over := len(raw) + len(fmt.Sprintf(`{"reason":,"first_ns":%d,"count":%d}`, r.FirstNS, r.Count)) - wire.MaxBodyBytes
		cut := max(len(r.Reason)-max(over, 1), 0)
		for cut > 0 && !utf8.RuneStart(r.Reason[cut]) {
			cut--
		}
		r.Reason = r.Reason[:cut]
	}
}

// ClearDegraded removes the degraded record (doctor --clear) for a caller that is not inside the
// section: it enters the section (giving up after 2 s), reads FORMAT, refuses to write when the
// format is not understood, reconciles if due, and removes the record. It must not be called
// while this process holds the section: entering again would fail.
func (s *Store) ClearDegraded() error {
	return s.inSideSection(func(sec *Section) error { return sec.ClearDegraded() })
}

func (s *Store) inSideSection(change func(*Section) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), sideSectionWait)
	defer cancel()
	sec, err := s.Enter(ctx)
	if err != nil {
		return fmt.Errorf("store: cannot enter the admission section: %w", err)
	}
	defer sec.Leave()
	n, understood, err := sec.Prepare(testhooks.Formats(), NowNS())
	if err != nil {
		return err
	}
	if !understood {
		return fmt.Errorf("store: %w", &FormatError{N: n, Suffix: "; not writing"})
	}
	return change(sec)
}
