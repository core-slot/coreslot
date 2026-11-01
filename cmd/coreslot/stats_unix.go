//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/needs"
	"github.com/core-slot/coreslot/internal/runstats"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// statsSchema is the version of the `stats --json` document.
const statsSchema = 1

// learned is what stats reads: the run log's statistics and the needs file, which is
// empty when it cannot be read, with needsErr saying why.
type learned struct {
	stats     []runstats.Stats
	skipped   int
	needsPath string
	needs     needs.File
	needsErr  error
}

// learn reads the run log under the state directory, summarizes the runs (only label's, when
// label is not empty) and loads the needs file. Each failure but the needs file's is a state
// error for the caller to report.
func learn(label string) (learned, error) {
	loc := locate()
	if loc.err != nil {
		return learned{}, fmt.Errorf("state directory: %w", loc.err)
	}
	st, err := store.OpenReadOnly(loc.dir)
	if err != nil {
		return learned{}, err
	}
	var l learned
	var runs []store.RunRecord
	if st.Exists() {
		if runs, l.skipped, err = st.ReadRuns(); err != nil {
			return learned{}, err
		}
	}
	if _, err := config.Load(loc.cfgPath); err != nil {
		return learned{}, err
	}
	for _, s := range runstats.Summarize(runs) {
		if label == "" || s.Label == label {
			l.stats = append(l.stats, s)
		}
	}
	l.needsPath = needs.PathFor(loc.cfgPath)
	l.needs, l.needsErr = needs.Load(l.needsPath)
	return l, nil
}

// needsNotice prints, for a needs file that cannot be read, the notice that ignores it.
func needsNotice(io IO, cmd string, l learned) {
	if l.needsErr != nil {
		fmt.Fprintf(io.Stderr, "coreslot: %s: needs file %s: %s; ignoring it\n", cmd, text(l.needsPath), text(l.needsErr.Error()))
	}
}

// parseOnly parses args with fs, which takes no operands. It returns false with the exit
// status when the command is done: help printed, or a usage error.
func parseOnly(fs *flag.FlagSet, args []string, name, usage string, io IO) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(io.Stdout, usage)
			return 0, false
		}
		fmt.Fprintf(io.Stderr, "coreslot: %s\n%s\n", text(err.Error()), usage)
		return exitUsage, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(io.Stderr, "coreslot: %s: unexpected arguments\n%s\n", name, usage)
		return exitUsage, false
	}
	return 0, true
}

// statsCommand is `coreslot stats [--label L] [--json]`: per label, what its last runs used and
// what they suggest. It exits 1 when the state directory, the run log or the configuration cannot
// be read (a state error, PROTOCOL.md section 19); a needs file that cannot be read is a notice,
// and every label's current entry is then none.
func statsCommand(args []string, io IO) int {
	fs, label, asJSON := statsFlags()
	if code, ok := parseOnly(fs, args, "stats", statsUsage, io); !ok {
		return code
	}
	l, err := learn(*label)
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: stats: %s\n", text(err.Error()))
		return exitProblems
	}
	needsNotice(io, "stats", l)
	if *asJSON {
		if err := writeStatsJSON(io, l); err != nil {
			fmt.Fprintf(io.Stderr, "coreslot: stats: %s\n", text(err.Error()))
			return exitInternal
		}
		return 0
	}
	var b strings.Builder
	if len(l.stats) == 0 {
		b.WriteString("no runs recorded\n")
	}
	line := func(key, value string) { fmt.Fprintf(&b, "  %s  %s\n", key, value) }
	for _, s := range l.stats {
		fmt.Fprintf(&b, "%s  %d %s\n", text(s.Label), s.Runs, plural(int64(s.Runs), "run", "runs"))
		line("window", s.Span())
		line("need", needText(s.NeedMiB))
		line("waited", fmt.Sprintf("%.0fs", s.WaitedS))
		line("current", currentText(l.needs, s.Label))
	}
	if l.skipped > 0 {
		fmt.Fprintf(&b, "skipped %d run-log %s that did not decode\n", l.skipped, plural(int64(l.skipped), "line", "lines"))
	}
	fmt.Fprint(io.Stdout, b.String())
	return 0
}

// needText is the need learned from a window, as the needs file would pin it.
func needText(mib int64) string {
	if mib == 0 {
		return "nothing measured"
	}
	return fmt.Sprintf("%s (mem = %q)", spec.FormatMiB(mib), spec.NeedText(mib))
}

func currentText(f needs.File, label string) string {
	e, ok, err := f.Lookup(label)
	switch {
	case err != nil:
		return "unreadable: " + text(err.Error())
	case !ok:
		return "none"
	}
	var parts []string
	if e.MemMiB > 0 {
		parts = append(parts, fmt.Sprintf("mem = %q (%s)", spec.NeedText(e.MemMiB), spec.FormatMiB(e.MemMiB)))
	}
	if e.NoRestart {
		parts = append(parts, "no_restart")
	}
	if len(parts) == 0 {
		return "an entry that sets nothing"
	}
	return strings.Join(parts, "  ")
}

type jsonStats struct {
	Schema       int              `json:"schema"`
	SkippedLines int              `json:"skipped_lines"`
	Labels       []jsonLabelStats `json:"labels"`
}

// jsonLabelStats is one label. The window's sums and peaks are over its last runs (window_runs);
// mem_peak_kib and need are null when no run recorded a peak.
type jsonLabelStats struct {
	Label      string       `json:"label"`
	Runs       int          `json:"runs"`
	WindowRuns int          `json:"window_runs"`
	FirstNS    int64        `json:"first_ns"`
	LastNS     int64        `json:"last_ns"`
	MemPeakKiB *int64       `json:"mem_peak_kib"`
	Need       *string      `json:"need"` // the need learned from the window
	WaitedS    float64      `json:"waited_s"`
	Current    *jsonCurrent `json:"current"` // null when the needs file has no entry
}

type jsonCurrent struct {
	Mem       *string `json:"mem"`
	NoRestart bool    `json:"no_restart"`
	Error     string  `json:"error,omitempty"`
}

func writeStatsJSON(io IO, l learned) error {
	doc := jsonStats{Schema: statsSchema, SkippedLines: l.skipped, Labels: []jsonLabelStats{}}
	for _, s := range l.stats {
		j := jsonLabelStats{Label: s.Label, Runs: s.Runs, WindowRuns: len(s.Window),
			FirstNS: s.Window[0].TimeNS, LastNS: s.Window[len(s.Window)-1].TimeNS, WaitedS: s.WaitedS}
		if s.MemPeakKiB > 0 {
			j.MemPeakKiB = &s.MemPeakKiB
		}
		if s.NeedMiB > 0 {
			j.Need = new(spec.NeedText(s.NeedMiB))
		}
		e, ok, err := l.needs.Lookup(s.Label)
		switch {
		case err != nil:
			j.Current = &jsonCurrent{Error: err.Error()}
		case ok:
			j.Current = &jsonCurrent{NoRestart: e.NoRestart}
			if e.MemMiB > 0 {
				j.Current.Mem = new(spec.NeedText(e.MemMiB))
			}
		}
		doc.Labels = append(doc.Labels, j)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.Stdout.Write(wire.EscapeJSON(buf.Bytes()))
	return err
}
