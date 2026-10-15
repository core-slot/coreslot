//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// statusSchema is the version of the --json document, carried as its "schema" member.
const statusSchema = 1

// observeEvery is how often `status --observe` reads the queue again.
const observeEvery = time.Second

// statusCommand is `coreslot status`: the budget, its holders and its queue, read without
// writing. It exits 3 while runs fail open (PROTOCOL.md sections 14 and 19): degraded mode is
// recorded, or the state directory, FORMAT or the configuration cannot be used.
func statusCommand(args []string, io IO) int {
	fs, asJSON, observe := statusFlags()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(io.Stdout, statusUsage)
			return 0
		}
		fmt.Fprintf(io.Stderr, "coreslot: %s\n%s\n", text(err.Error()), statusUsage)
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(io.Stderr, "coreslot: status: unexpected arguments\n%s\n", statusUsage)
		return exitUsage
	}
	if *observe < 0 {
		fmt.Fprintf(io.Stderr, "coreslot: status: --observe %s: want a duration of 0 or more\n%s\n", *observe, statusUsage)
		return exitUsage
	}
	if *observe > 0 && *observe < state.StallAfter {
		fmt.Fprintf(io.Stderr, "coreslot: status: --observe %s is shorter than %s, so no ticket can be found STALLED; use %s or more\n",
			*observe, state.StallAfter, state.StallAfter)
	}
	e, err := openEngine(locate(), true)
	if err != nil {
		return statusFailed(err, io)
	}
	rep, w, err := observeQueue(e.Snapshot, *observe, newRealClock())
	if err != nil {
		return statusFailed(err, io)
	}
	v := buildStatus(rep, store.NowNS(), w, *observe)
	if *asJSON {
		if err := v.writeJSON(io.Stdout); err != nil {
			fmt.Fprintf(io.Stderr, "coreslot: status: %s\n", text(err.Error()))
			return exitInternal
		}
	} else {
		fmt.Fprint(io.Stdout, v.human())
	}
	if rep.Degraded != nil || rep.FormatError != "" || rep.ConfigError != "" {
		return exitDegraded
	}
	return 0
}

// statusFailed reports an error that kept status from reading the state. A state error
// (PROTOCOL.md section 14) exits 3, like a recorded degraded mode: runs fail open while it lasts.
// Anything else is an internal error, exit 70.
func statusFailed(err error, io IO) int {
	if _, stateErr := classify(err); stateErr {
		fmt.Fprintf(io.Stderr, "coreslot: status: %s; runs fail open until it is fixed (`coreslot doctor` checks it)\n", text(err.Error()))
		return exitDegraded
	}
	fmt.Fprintf(io.Stderr, "coreslot: status: %s\n", text(err.Error()))
	return exitInternal
}

// stallWatcher judges freshness the way a waiter does (PROTOCOL.md section 10.2): it remembers
// the last mtime it saw of each ticket and when, on its own monotonic clock, that value last
// changed. It never compares an mtime with a clock.
type stallWatcher struct {
	obs state.Observer
	now time.Duration // the observer's time at the last sighting
}

func ticketKey(t engine.TicketInfo) (state.Key, bool) {
	id, ok := wire.ParseID(t.ID)
	return state.Key(id), ok
}

func (w *stallWatcher) see(rep engine.Report, now time.Duration) {
	w.now = now
	for _, t := range rep.Tickets {
		if k, ok := ticketKey(t); ok {
			w.obs.Observe(k, t.MtimeNS, now)
		}
	}
}

func (w *stallWatcher) stalled(t engine.TicketInfo) bool {
	k, ok := ticketKey(t)
	return ok && w.obs.Stalled(k, w.now)
}

// observeQueue takes the snapshot and, when window is positive, takes it again every second for
// window, feeding each sighting to a stallWatcher. A ticket whose mtime stayed the same for
// state.StallAfter of that observation is STALLED for this observer; a single reading cannot
// tell, since a first sighting never is. It returns the last snapshot. The observation runs on
// the wait loop's clock; nothing ends its sleeps early.
func observeQueue(snap func() (engine.Report, error), window time.Duration, clk waitClock) (engine.Report, *stallWatcher, error) {
	w := &stallWatcher{}
	rep, err := snap()
	if err != nil {
		return rep, w, err
	}
	start := clk.Now()
	w.see(rep, start) // the window runs from the first sighting, so a ticket seen then can reach StallAfter in it
	for clk.Now()-start < window {
		_ = clk.Sleep(context.Background(), min(observeEvery, window-(clk.Now()-start)))
		if rep, err = snap(); err != nil {
			return rep, w, err
		}
		w.see(rep, clk.Now())
	}
	return rep, w, nil
}

// ---------------------------------------------------------------- the view

type ticketView struct {
	info     engine.TicketInfo
	state    string // waiting, stalled or dead
	position int    // 1 is the head; 0 for a dead ticket
	waited   time.Duration
}

// statusView is a snapshot with what status derives from it: ticket positions and states,
// ages, and the locked tokens no hold lists.
type statusView struct {
	rep      engine.Report
	nowNS    int64
	observed time.Duration
	tickets  []ticketView
	unlisted map[string]int // resource -> locked tokens that no live hold lists
}

func buildStatus(rep engine.Report, nowNS int64, w *stallWatcher, observed time.Duration) statusView {
	v := statusView{rep: rep, nowNS: nowNS, observed: observed, unlisted: map[string]int{}}
	ahead := 0 // live, non-stalled tickets in front, the way a waiter counts its position
	for _, t := range rep.Tickets {
		tv := ticketView{info: t, state: "waiting", waited: time.Duration(max(nowNS-t.CreatedNS, 0))}
		switch {
		case t.Dead:
			tv.state = "dead"
		default:
			tv.position = ahead + 1
			if w != nil && w.stalled(t) {
				tv.state = "stalled"
			} else {
				ahead++
			}
		}
		v.tickets = append(v.tickets, tv)
	}
	for name, r := range rep.Resources {
		listed := 0
		for _, h := range rep.Holds {
			listed += h.Grant[name]
		}
		if n := r.Locked - listed; n > 0 {
			v.unlisted[name] = n
		}
	}
	return v
}

// holderAlive reports whether the process admit.lock names still exists (kill with signal 0:
// a process of another user exists too, which EPERM reports).
func holderAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

func sectionHeldFor(s engine.SectionInfo, nowNS int64) time.Duration {
	if !s.Held || s.EnteredNS == 0 {
		return 0
	}
	return time.Duration(max(nowNS-s.EnteredNS, 0))
}

// sectionHolder describes the admission section from admit.lock's informational body, which
// may be stale: `free`, or `held by pid 4711 for 3s (as recorded in admit.lock)`.
func sectionHolder(s engine.SectionInfo, nowNS int64) string {
	switch {
	case !s.Held:
		return "free"
	case s.PID != 0 && !holderAlive(s.PID):
		return fmt.Sprintf("held; holder unknown (last recorded pid %d has exited)", s.PID)
	case s.PID != 0:
		return fmt.Sprintf("held by pid %d for %s (as recorded in admit.lock)", s.PID, duration(sectionHeldFor(s, nowNS)))
	}
	return "held (the holder is not recorded)"
}

// ---------------------------------------------------------------- human output

func labelText(label string) string {
	if label == "" {
		return "(no label)"
	}
	return text(label)
}

func (v statusView) human() string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	r := v.rep
	p("state directory: %s", text(r.StateDir))
	if !r.StateDirExists {
		p("no state directory yet (nothing has run)")
	}
	switch {
	case r.FormatError != "":
		p("format: MALFORMED (%s)", text(r.FormatError))
	case !r.FormatUnderstood: // before absence: a FORMAT naming format 0 is present, not absent
		p("format: %d, which this client does not implement (token-only mode)", r.Format)
	case !r.FormatPresent:
		p("format: not written yet (the first client writes 1)")
	default:
		p("format: %d", r.Format)
	}
	p("admission section: %s", sectionHolder(r.Section, v.nowNS))
	if d := r.Degraded; d != nil {
		if d.Malformed {
			p("DEGRADED: the record is unreadable; `coreslot doctor --clear` removes it")
		} else {
			p("DEGRADED: %s (%d fail-open %s since %s); `coreslot doctor --clear` removes the record",
				text(d.Reason), d.Count, plural(d.Count, "run", "runs"), time.Unix(0, d.FirstNS).UTC().Format(time.RFC3339))
		}
	} else {
		p("degraded: no")
	}
	if r.ConfigError != "" {
		// The file's own error already begins with "configuration <path>:", so under the label
		// it goes on from the path; any other reason (the CPU count, say) is printed whole.
		p("CONFIGURATION ERROR: %s", text(strings.TrimPrefix(r.ConfigError, "configuration ")))
	}

	if r.ConfigError != "" {
		p("resources: not known, the configuration cannot be read (runs fail open until it is fixed)")
	} else {
		p("resources:")
	}
	for _, name := range slices.Sorted(maps.Keys(r.Resources)) {
		ri := r.Resources[name]
		line := fmt.Sprintf("  %s  size %d  locked %d  free %d", name, ri.Size, ri.Locked, ri.Free)
		if n := v.unlisted[name]; n > 0 {
			line += fmt.Sprintf("  (%d locked, no hold lists %s)", n, plural(int64(n), "it", "them"))
		}
		p("%s", line)
	}

	if len(r.Holds) == 0 {
		p("holds: none")
	} else {
		p("holds:")
	}
	for _, h := range r.Holds {
		line := fmt.Sprintf("  %s  pid %d  %s  held %s", labelText(h.Label), h.PID, holdTokens(h.Grant),
			duration(time.Duration(max(v.nowNS-h.StartNS, 0))))
		if h.Adopted {
			line += "  adopted"
		}
		if len(h.Clamped) > 0 {
			var parts []string
			for _, name := range slices.Sorted(maps.Keys(h.Clamped)) {
				parts = append(parts, fmt.Sprintf("%s (asked %d)", name, h.Clamped[name]))
			}
			line += "  clamped " + strings.Join(parts, ", ")
		}
		switch {
		case len(h.UnbudgetedResources) > 0:
			line += "  unbudgeted " + strings.Join(h.UnbudgetedResources, ", ")
		case h.Unbudgeted:
			line += "  unbudgeted"
		}
		p("%s", line)
	}

	if len(r.UndeclaredLocked) > 0 {
		p("undeclared resources with locked tokens (not in the configuration): %s", tokenList(r.UndeclaredLocked))
	}

	if len(v.unlisted) > 0 {
		p("locked tokens no hold lists: %s (token-only runs, or a hold in transition)", tokenList(v.unlisted))
	}

	if len(v.tickets) == 0 {
		p("queue: empty")
	} else {
		p("queue:")
	}
	for _, t := range v.tickets {
		pos := "-"
		if t.position > 0 {
			pos = fmt.Sprint(t.position)
		}
		line := fmt.Sprintf("  %s. %s  pid %d  priority %d  need %s  waiting %s", pos, labelText(t.info.Label),
			t.info.PID, t.info.Priority, needText(t.info.Need), duration(t.waited))
		switch t.state {
		case "stalled":
			line += "  STALLED"
		case "dead":
			line += "  dead (withdrawn, or its owner exited)"
		}
		p("%s  id %s", line, t.info.ID)
	}
	switch {
	case v.observed > 0:
		p("queue observed for %s", duration(v.observed))
	case len(v.tickets) > 0:
		p("STALLED: not observed (use --observe %s or more)", state.StallAfter)
	}

	if n := len(r.DeadHolds); n > 0 {
		p("dead holds awaiting reconciliation: %d", n)
	}
	if n := len(r.Cancels); n > 0 {
		p("pending cancel requests: %d", n)
	}
	if ignored := ignoredText(r.Ignored); ignored != "" {
		p("ignored entries (malformed or stray): %s", ignored)
	}
	return b.String()
}

// plural is one when n is 1, else many.
func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// holdTokens is `cpu 12, gpu 1`, or `no tokens` for an unbudgeted hold.
func holdTokens(grant map[string]int) string {
	if len(grant) == 0 {
		return "no tokens"
	}
	return tokenList(grant)
}

// tokenList is a count of tokens per resource in name order: `cpu 12, gpu 1`.
func tokenList(tokens map[string]int) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(tokens)) {
		parts = append(parts, fmt.Sprintf("%s %d", name, tokens[name]))
	}
	return strings.Join(parts, ", ")
}

// ignoredText lists the entries ignored per directory of the layout, `queue 1, holds 2`, leaving
// out a directory with none; "" when nothing was ignored.
func ignoredText(ignored map[string]int) string {
	var parts []string
	for _, dir := range []string{store.QueueDir, store.HoldsDir, store.CancelDir} {
		if n := ignored[dir]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", dir, n))
		}
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------- JSON output

type jsonStatus struct {
	Schema           int                     `json:"schema"`
	StateDir         string                  `json:"state_dir"`
	StateDirExists   bool                    `json:"state_dir_exists"`
	Format           int                     `json:"format"`
	FormatPresent    bool                    `json:"format_present"`
	FormatUnderstood bool                    `json:"format_understood"`
	FormatError      string                  `json:"format_error"`
	Gen              int64                   `json:"gen"`
	ReconciledNS     int64                   `json:"reconciled_ns"`
	ConfigError      string                  `json:"config_error"`
	Degraded         *jsonDegraded           `json:"degraded"`
	Section          jsonSection             `json:"section"`
	Resources        map[string]jsonResource `json:"resources"`
	UndeclaredLocked map[string]int          `json:"undeclared_locked"`
	Holds            []jsonHold              `json:"holds"`
	Tickets          []jsonTicket            `json:"tickets"`
	DeadHolds        []string                `json:"dead_holds"`
	CancelRequests   []string                `json:"cancel_requests"`
	Ignored          map[string]int          `json:"ignored"`
	ObservedS        float64                 `json:"observed_s"`
}

type jsonDegraded struct {
	Reason    string `json:"reason"`
	FirstNS   int64  `json:"first_ns"`
	Count     int64  `json:"count"`
	Malformed bool   `json:"malformed"`
}

// jsonSection is the admission section. Holder is null while the section is free. Its fields
// come from admit.lock's informational body: PID and EnteredNS are 0 when it is unreadable,
// and Alive (kill with signal 0) is null then; a holder that is not alive is a stale record.
type jsonSection struct {
	Held   bool        `json:"held"`
	Holder *jsonHolder `json:"holder"`
}

type jsonHolder struct {
	PID       int     `json:"pid"`
	EnteredNS int64   `json:"entered_ns"`
	HeldForS  float64 `json:"held_for_s"`
	Alive     *bool   `json:"alive"`
}

type jsonResource struct {
	Size           int `json:"size"`
	Locked         int `json:"locked"`
	Free           int `json:"free"`
	UnlistedLocked int `json:"unlisted_locked"`
}

type jsonHold struct {
	Name                string         `json:"name"`
	Label               string         `json:"label"`
	PID                 int            `json:"pid"`
	StartNS             int64          `json:"start_ns"`
	HeldForS            float64        `json:"held_for_s"`
	Grant               map[string]int `json:"grant"`
	Unbudgeted          bool           `json:"unbudgeted"`
	UnbudgetedResources []string       `json:"unbudgeted_resources"`
	Clamped             map[string]int `json:"clamped"`
	Adopted             bool           `json:"adopted"`
}

type jsonRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type jsonTicket struct {
	ID        string               `json:"id"`
	Label     string               `json:"label"`
	PID       int                  `json:"pid"`
	Priority  int                  `json:"priority"`
	Need      map[string]jsonRange `json:"need"`
	CreatedNS int64                `json:"created_ns"`
	WaitedS   float64              `json:"waited_s"`
	Position  int                  `json:"position"` // 1 is the head; 0 for a dead ticket
	State     string               `json:"state"`    // waiting, stalled or dead
	Stalled   *bool                `json:"stalled"`  // null unless --observe ran
}

func (v statusView) document() jsonStatus {
	r := v.rep
	d := jsonStatus{
		Schema: statusSchema, StateDir: r.StateDir, StateDirExists: r.StateDirExists, Format: r.Format, FormatPresent: r.FormatPresent,
		FormatUnderstood: r.FormatUnderstood, FormatError: r.FormatError, Gen: r.Gen, ReconciledNS: r.ReconciledNS, ConfigError: r.ConfigError,
		Section:   jsonSection{Held: r.Section.Held},
		Resources: map[string]jsonResource{}, UndeclaredLocked: maps.Clone(r.UndeclaredLocked), Holds: []jsonHold{}, Tickets: []jsonTicket{},
		DeadHolds: nonNil(r.DeadHolds), CancelRequests: nonNil(r.Cancels),
		Ignored: map[string]int{}, ObservedS: v.observed.Seconds(),
	}
	if d.UndeclaredLocked == nil {
		d.UndeclaredLocked = map[string]int{}
	}
	if r.Section.Held {
		h := &jsonHolder{PID: r.Section.PID, EnteredNS: r.Section.EnteredNS, HeldForS: sectionHeldFor(r.Section, v.nowNS).Seconds()}
		if r.Section.PID != 0 {
			alive := holderAlive(r.Section.PID)
			h.Alive = &alive
		}
		d.Section.Holder = h
	}
	if x := r.Degraded; x != nil {
		d.Degraded = &jsonDegraded{Reason: x.Reason, FirstNS: x.FirstNS, Count: x.Count, Malformed: x.Malformed}
	}
	for name, ri := range r.Resources {
		d.Resources[name] = jsonResource{Size: ri.Size, Locked: ri.Locked, Free: ri.Free, UnlistedLocked: v.unlisted[name]}
	}
	for _, dir := range []string{store.QueueDir, store.HoldsDir, store.CancelDir} {
		d.Ignored[dir] = r.Ignored[dir]
	}
	for _, h := range r.Holds {
		d.Holds = append(d.Holds, jsonHold{Name: h.Name, Label: h.Label, PID: h.PID, StartNS: h.StartNS,
			HeldForS: time.Duration(max(v.nowNS-h.StartNS, 0)).Seconds(), Grant: nonNilMap(h.Grant),
			Unbudgeted: h.Unbudgeted, UnbudgetedResources: nonNil(h.UnbudgetedResources),
			Clamped: nonNilMap(h.Clamped), Adopted: h.Adopted})
	}
	for _, t := range v.tickets {
		need := make(map[string]jsonRange, len(t.info.Need))
		for name, mm := range t.info.Need {
			need[name] = jsonRange{Min: mm.Min, Max: mm.Max}
		}
		jt := jsonTicket{ID: t.info.ID, Label: t.info.Label, PID: t.info.PID,
			Priority: t.info.Priority, Need: need, CreatedNS: t.info.CreatedNS, WaitedS: t.waited.Seconds(),
			Position: t.position, State: t.state}
		if v.observed > 0 {
			stalled := t.state == "stalled"
			jt.Stalled = &stalled
		}
		d.Tickets = append(d.Tickets, jt)
	}
	return d
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilMap(m map[string]int) map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m
}

// writeJSON writes the document with labels exactly as stored: encoding/json escapes the C0
// controls, wire.EscapeJSON the other characters that act on a terminal (C1 controls, bidi
// controls), and every valid UTF-8 label decodes to the same string.
func (v statusView) writeJSON(w interface{ Write([]byte) (int, error) }) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v.document()); err != nil {
		return err
	}
	_, err := w.Write(wire.EscapeJSON(buf.Bytes()))
	return err
}
