//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/needs"
	"github.com/core-slot/coreslot/internal/runstats"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// statsSchema is the version of the `stats --json` document.
const statsSchema = 1

// learned is what stats and suggest read: the run log's statistics and the needs file, which is
// empty when it cannot be read, with needsErr saying why.
type learned struct {
	stats     []runstats.Stats
	skipped   int
	needsPath string
	needs     needs.File
	needsErr  error
}

// learn reads the run log under the state directory, sizes cpu from the configuration and the
// machine, summarizes the runs (only label's base, when label is not empty) and loads the needs
// file. Each failure but the needs file's is a state error for the caller to report.
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
	cfg, err := config.Load(loc.cfgPath)
	if err != nil {
		return learned{}, err
	}
	m, err := config.ReadMachine(false) // only the cpu size is read below
	if err != nil {
		return learned{}, fmt.Errorf("machine: %w", err)
	}
	for _, s := range runstats.Summarize(runs, cfg.Sizes(m)["cpu"]) {
		if label == "" || s.Label == needs.BaseLabel(label) {
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

// cpuText is a cpu need as the needs file writes it: "N" for N..N, else "MIN..MAX".
func cpuText(r spec.Range) string {
	if r.Min == r.Max {
		return fmt.Sprint(r.Min)
	}
	return fmt.Sprintf("%d..%d", r.Min, r.Max)
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
		line("evidence", text(s.Suggest.Reason))
		if len(s.CoresUsed) > 0 {
			cores := make([]string, len(s.CoresUsed))
			for i, c := range s.CoresUsed {
				cores[i] = fmt.Sprintf("%.1f", c)
			}
			line("cores used", strings.Join(cores, " "))
		}
		line("waited", fmt.Sprintf("%.0fs, %.0fs of it while the memory was free", s.WaitedS, s.WaitedFreeS))
		line("suggest", suggestionText(s.Suggest))
		line("current", currentText(l.needs, s.Label))
	}
	if l.skipped > 0 {
		fmt.Fprintf(&b, "skipped %d run-log %s that did not decode\n", l.skipped, plural(int64(l.skipped), "line", "lines"))
	}
	fmt.Fprint(io.Stdout, b.String())
	return 0
}

func suggestionText(sg runstats.Suggestion) string {
	var parts []string
	if sg.CPU != nil {
		parts = append(parts, fmt.Sprintf("cpu = %q", cpuText(*sg.CPU)))
	}
	if sg.MemMiB > 0 {
		parts = append(parts, fmt.Sprintf("mem = %q", spec.NeedText(sg.MemMiB)))
	}
	if len(parts) == 0 {
		return "nothing measured"
	}
	return strings.Join(parts, "  ")
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
	if e.CPU != nil {
		parts = append(parts, fmt.Sprintf("cpu = %q", cpuText(*e.CPU)))
	}
	if e.PerWorker {
		parts = append(parts, "cpu per worker")
	}
	if e.MemMiB > 0 {
		parts = append(parts, fmt.Sprintf("mem = %q (%s)", spec.NeedText(e.MemMiB), spec.FormatMiB(e.MemMiB)))
	}
	if e.Pinned {
		parts = append(parts, "pinned")
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
// mem_peak_kib and peak_after_s are null when no run recorded them.
type jsonLabelStats struct {
	Label         string         `json:"label"`
	Runs          int            `json:"runs"`
	WindowRuns    int            `json:"window_runs"`
	FirstNS       int64          `json:"first_ns"`
	LastNS        int64          `json:"last_ns"`
	CoresUsed     []float64      `json:"cores_used"`
	Saturated     int            `json:"saturated"`
	MemPeakKiB    *int64         `json:"mem_peak_kib"`
	MemPeakSource string         `json:"mem_peak_source"`
	PeakAfterS    *float64       `json:"peak_after_s"`
	WaitedS       float64        `json:"waited_s"`
	WaitedFreeS   float64        `json:"waited_free_s"`
	Suggestion    jsonSuggestion `json:"suggestion"`
	Current       *jsonCurrent   `json:"current"` // null when the needs file has no entry
}

type jsonSuggestion struct {
	CPU      *string `json:"cpu"`
	Mem      *string `json:"mem"`
	Evidence string  `json:"evidence"`
}

type jsonCurrent struct {
	CPU          *string `json:"cpu"`
	Mem          *string `json:"mem"`
	CPUPerWorker bool    `json:"cpu_per_worker"`
	Pinned       bool    `json:"pinned"`
	Error        string  `json:"error,omitempty"`
}

func writeStatsJSON(io IO, l learned) error {
	doc := jsonStats{Schema: statsSchema, SkippedLines: l.skipped, Labels: []jsonLabelStats{}}
	for _, s := range l.stats {
		j := jsonLabelStats{Label: s.Label, Runs: s.Runs, WindowRuns: len(s.Window),
			FirstNS: s.Window[0].TimeNS, LastNS: s.Window[len(s.Window)-1].TimeNS,
			CoresUsed: s.CoresUsed, Saturated: s.Saturated, MemPeakSource: s.MemPeakSource,
			PeakAfterS: s.PeakAfterS, WaitedS: s.WaitedS, WaitedFreeS: s.WaitedFreeS,
			Suggestion: jsonSuggestion{Evidence: s.Suggest.Reason}}
		if j.CoresUsed == nil {
			j.CoresUsed = []float64{}
		}
		if s.MemPeakKiB > 0 {
			j.MemPeakKiB = &s.MemPeakKiB
		}
		if s.Suggest.CPU != nil {
			j.Suggestion.CPU = new(cpuText(*s.Suggest.CPU))
		}
		if s.Suggest.MemMiB > 0 {
			j.Suggestion.Mem = new(spec.NeedText(s.Suggest.MemMiB))
		}
		e, ok, err := l.needs.Lookup(s.Label)
		switch {
		case err != nil:
			j.Current = &jsonCurrent{Error: err.Error()}
		case ok:
			j.Current = &jsonCurrent{CPUPerWorker: e.PerWorker, Pinned: e.Pinned}
			if e.CPU != nil {
				j.Current.CPU = new(cpuText(*e.CPU))
			}
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

// suggestion is what suggest does with one label: the keys it sets, or why it sets none.
type suggestion struct {
	label string
	set   map[string]any
	kept  string // why the entry is kept as it is, when set is nil
}

// suggestions decides, per label, the keys to set: mem, cpu unless f's entry gives cpu per
// worker, and the evidence. A pinned entry, or one that does not parse, is kept.
func suggestions(stats []runstats.Stats, f needs.File) []suggestion {
	var out []suggestion
	for _, s := range stats {
		sg := suggestion{label: s.Label}
		e, _, err := f.Lookup(s.Label)
		switch {
		case err != nil:
			sg.kept = "kept, its entry does not parse: " + err.Error()
		case e.Pinned:
			sg.kept = "pinned, kept"
		case s.Suggest.CPU == nil && s.Suggest.MemMiB == 0:
			sg.kept = "nothing measured, kept"
		default:
			sg.set = map[string]any{"evidence": s.Suggest.Reason}
			if s.Suggest.CPU != nil && !e.PerWorker {
				sg.set["cpu"] = cpuText(*s.Suggest.CPU)
			}
			if s.Suggest.MemMiB > 0 {
				sg.set["mem"] = spec.NeedText(s.Suggest.MemMiB)
			}
		}
		out = append(out, sg)
	}
	return out
}

// suggestCommand is `coreslot suggest [--label L] [--write]`: it prints the needs-file entries
// the run log suggests, and with --write sets them in the needs file, keeping every other key
// and every pinned entry, as the file reads under its lock. It exits 1 when what it reads cannot
// be read or the needs file cannot be written (a state error, PROTOCOL.md section 19); a
// malformed needs file is left as it is. Without --write, a needs file that cannot be read is a
// notice, as in stats.
func suggestCommand(args []string, io IO) int {
	fs, label, write := suggestFlags()
	if code, ok := parseOnly(fs, args, "suggest", suggestUsage, io); !ok {
		return code
	}
	l, err := learn(*label)
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: suggest: %s\n", text(err.Error()))
		return exitProblems
	}
	if *write && l.needsErr != nil {
		fmt.Fprintf(io.Stderr, "coreslot: suggest: needs file %s: %s\n", text(l.needsPath), text(l.needsErr.Error()))
		return exitProblems
	}
	needsNotice(io, "suggest", l)
	if len(l.stats) == 0 {
		fmt.Fprintln(io.Stdout, "no runs recorded")
		return 0
	}
	if !*write {
		return printSuggestions(io, l.needsPath, suggestions(l.stats, l.needs))
	}
	var sgs []suggestion
	err = needs.Update(l.needsPath, func(entries map[string]map[string]any) error {
		sgs = suggestions(l.stats, needs.FromEntries(entries))
		for _, sg := range sgs {
			if sg.set == nil {
				continue
			}
			e := entries[sg.label]
			if e == nil {
				e = map[string]any{}
				entries[sg.label] = e
			}
			for k, v := range sg.set {
				e[k] = v
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: suggest: needs file %s: %s\n", text(l.needsPath), text(err.Error()))
		return exitProblems
	}
	for _, sg := range sgs {
		if sg.set == nil {
			fmt.Fprintf(io.Stdout, "%s: %s\n", text(sg.label), text(sg.kept))
			continue
		}
		var parts []string
		for _, k := range []string{"cpu", "mem"} {
			if v, ok := sg.set[k]; ok {
				parts = append(parts, fmt.Sprintf("%s = %q", k, v))
			}
		}
		fmt.Fprintf(io.Stdout, "%s: wrote %s and its evidence\n", text(sg.label), strings.Join(parts, ", "))
	}
	return 0
}

// printSuggestions prints, as TOML, the keys --write would set, and as comments the entries it
// would keep.
func printSuggestions(io IO, path string, sgs []suggestion) int {
	set := map[string]map[string]any{}
	var b strings.Builder
	fmt.Fprintf(&b, "# coreslot suggest --write sets these keys in %s; it keeps the others\n", text(path))
	for _, sg := range sgs {
		if sg.set == nil {
			fmt.Fprintf(&b, "# %s: %s\n", text(sg.label), text(sg.kept))
		} else {
			set[sg.label] = sg.set
		}
	}
	if len(set) > 0 {
		b.WriteString("\n")
		if err := toml.NewEncoder(&b).Encode(map[string]any{"needs": set}); err != nil {
			fmt.Fprintf(io.Stderr, "coreslot: suggest: %s\n", text(err.Error()))
			return exitInternal
		}
	}
	fmt.Fprint(io.Stdout, b.String())
	return 0
}
