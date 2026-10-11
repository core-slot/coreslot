//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/Nezhinskiy/coreslot"
	"github.com/Nezhinskiy/coreslot/internal/state"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

const statusUsage = `usage: coreslot status [--json] [--observe DURATION]`

// statusSchema is the version of the --json document.
const statusSchema = 1

// observeEvery is how often `status --observe` reads the queue again.
const observeEvery = time.Second

// statusCommand is `coreslot status`: the budget, its holders and its queue, read without
// writing. It exits 3 while degraded mode is recorded (PROTOCOL.md section 14).
func statusCommand(args []string, io IO) int {
	fs := flag.NewFlagSet("coreslot status", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	asJSON := fs.Bool("json", false, "print the JSON document (schema 1)")
	observe := fs.Duration("observe", 0, "watch the queue this long, to find STALLED tickets")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(io.Stdout, statusUsage)
			return 0
		}
		fmt.Fprintf(io.Stderr, "coreslot: %v\n%s\n", err, statusUsage)
		return exitUsage
	}
	if fs.NArg() > 0 || *observe < 0 {
		fmt.Fprintf(io.Stderr, "coreslot: status: unexpected arguments\n%s\n", statusUsage)
		return exitUsage
	}
	c, err := coreslot.Open(coreslot.Options{})
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: status: %v\n", err)
		return exitInternal
	}
	snap := func() (rep coreslot.Report, err error) {
		err = inSection(func() (err error) { // Snapshot probes admit.lock
			rep, err = c.Snapshot()
			return err
		})
		return rep, err
	}
	rep, w, err := observeQueue(snap, *observe, monotonicClock())
	if err != nil {
		fmt.Fprintf(io.Stderr, "coreslot: status: %v\n", err)
		return exitInternal
	}
	v := buildStatus(rep, store.NowNS(), w, *observe)
	if *asJSON {
		if err := v.writeJSON(io.Stdout); err != nil {
			fmt.Fprintf(io.Stderr, "coreslot: status: %v\n", err)
			return exitInternal
		}
	} else {
		fmt.Fprint(io.Stdout, v.human())
	}
	if rep.Degraded != nil {
		return exitDegraded
	}
	return 0
}

// obsClock is the clock of an observation: the observer's own monotonic time, and a sleep.
type obsClock struct {
	now   func() time.Duration
	sleep func(time.Duration)
}

func monotonicClock() obsClock {
	start := time.Now()
	return obsClock{now: func() time.Duration { return time.Since(start) }, sleep: time.Sleep}
}

// stallWatcher judges freshness the way a waiter does (PROTOCOL.md section 10.2): it remembers
// the last mtime it saw of each ticket and when, on its own monotonic clock, that value last
// changed. It never compares an mtime with a clock.
type stallWatcher struct {
	obs state.Observer
	now time.Duration // the observer's time at the last sighting
}

func ticketKey(t coreslot.TicketInfo) (state.Key, bool) {
	id, ok := wire.ParseID(t.ID)
	return state.Key{OrderKey: id.OrderKey, PID: id.PID, Seq: id.Seq}, ok
}

func (w *stallWatcher) see(rep coreslot.Report, now time.Duration) {
	w.now = now
	for _, t := range rep.Tickets {
		if k, ok := ticketKey(t); ok {
			w.obs.Observe(k, t.MtimeNS, now)
		}
	}
}

func (w *stallWatcher) stalled(t coreslot.TicketInfo) bool {
	k, ok := ticketKey(t)
	return ok && w.obs.Stalled(k, w.now)
}

// observeQueue takes the snapshot and, when window is positive, takes it again every second for
// window, feeding each sighting to a stallWatcher. A ticket whose mtime stayed the same for
// state.StallAfter of that observation is STALLED for this observer; a single reading cannot
// tell, since a first sighting never is. It returns the last snapshot.
func observeQueue(snap func() (coreslot.Report, error), window time.Duration, clk obsClock) (coreslot.Report, *stallWatcher, error) {
	w := &stallWatcher{}
	start := clk.now()
	rep, err := snap()
	if err != nil {
		return rep, w, err
	}
	w.see(rep, clk.now())
	for clk.now()-start < window {
		clk.sleep(min(observeEvery, window-(clk.now()-start)))
		if rep, err = snap(); err != nil {
			return rep, w, err
		}
		w.see(rep, clk.now())
	}
	return rep, w, nil
}

// ---------------------------------------------------------------- the view

type ticketView struct {
	info     coreslot.TicketInfo
	state    string // waiting, stalled or dead
	position int    // 1 is the head; 0 for a dead ticket
	waited   time.Duration
}

// statusView is a snapshot with what status derives from it: ticket positions and states,
// ages, and the locked tokens no hold lists.
type statusView struct {
	rep      coreslot.Report
	nowNS    int64
	observed time.Duration
	tickets  []ticketView
	unlisted map[string]int // resource -> locked tokens that no live hold lists
}

func buildStatus(rep coreslot.Report, nowNS int64, w *stallWatcher, observed time.Duration) statusView {
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

func (v statusView) sectionFor() time.Duration {
	if !v.rep.Section.Held || v.rep.Section.EnteredNS == 0 {
		return 0
	}
	return time.Duration(max(v.nowNS-v.rep.Section.EnteredNS, 0))
}

// ---------------------------------------------------------------- human output

// text escapes user text so that an entry stays on one line.
func text(s string) string { return wire.EscapeHuman(s) }

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
	switch {
	case r.FormatError != "":
		p("format: MALFORMED (%s)", text(r.FormatError))
	case r.Format == 0:
		p("format: not written yet (the first client writes 1)")
	case !r.FormatUnderstood:
		p("format: %d, which this client does not implement (token-only mode)", r.Format)
	default:
		p("format: %d", r.Format)
	}
	switch {
	case !r.Section.Held:
		p("admission section: free")
	case r.Section.PID != 0:
		p("admission section: held by pid %d for %s (as recorded in admit.lock)", r.Section.PID, duration(v.sectionFor()))
	default:
		p("admission section: held (the holder is not recorded)")
	}
	if d := r.Degraded; d != nil {
		if d.Malformed {
			p("DEGRADED: the record is unreadable; `coreslot doctor --clear` removes it")
		} else {
			p("DEGRADED: %s (%d fail-open %s since %s); `coreslot doctor --clear` removes the record",
				text(d.Reason), d.Count, plural(d.Count, "run"), time.Unix(0, d.FirstNS).UTC().Format(time.RFC3339))
		}
	} else {
		p("degraded: no")
	}
	if r.ConfigError != "" {
		p("CONFIGURATION ERROR: %s", text(r.ConfigError))
	}

	p("resources:")
	if len(r.Resources) == 0 {
		p("  none declared")
	}
	for _, name := range slices.Sorted(maps.Keys(r.Resources)) {
		ri := r.Resources[name]
		line := fmt.Sprintf("  %s  size %d  locked %d  free %d", name, ri.Size, ri.Locked, ri.Free)
		if n := v.unlisted[name]; n > 0 {
			line += fmt.Sprintf("  (%d locked, no hold lists %s)", n, pluralWord(n, "it", "them"))
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

	if len(v.unlisted) > 0 {
		var parts []string
		for _, name := range slices.Sorted(maps.Keys(v.unlisted)) {
			parts = append(parts, fmt.Sprintf("%s %d", name, v.unlisted[name]))
		}
		p("locked tokens no hold lists: %s (token-only runs, or a hold in transition)", strings.Join(parts, ", "))
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
			line += "  dead (the owner is gone)"
		}
		p("%s  id %s", line, t.info.ID)
	}
	if v.observed > 0 {
		p("queue observed for %s", duration(v.observed))
	}

	if n := len(r.DeadHolds); n > 0 {
		p("dead holds awaiting reconciliation: %d", n)
	}
	if n := len(r.Cancels); n > 0 {
		p("pending cancel requests: %d", n)
	}
	var ignored []string
	for _, dir := range []string{store.QueueDir, store.HoldsDir, store.CancelDir} {
		if n := r.Ignored[dir]; n > 0 {
			ignored = append(ignored, fmt.Sprintf("%s %d", dir, n))
		}
	}
	if len(ignored) > 0 {
		p("ignored entries (malformed or stray): %s", strings.Join(ignored, ", "))
	}
	return b.String()
}

func plural(n int64, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func pluralWord(n int, one, many string) string {
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
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(grant)) {
		parts = append(parts, fmt.Sprintf("%s %d", name, grant[name]))
	}
	return strings.Join(parts, ", ")
}

// needText is a ticket's request as `cpu 4..12, gpu 1..1`.
func needText(need map[string][2]int) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(need)) {
		parts = append(parts, fmt.Sprintf("%s %d..%d", name, need[name][0], need[name][1]))
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------- JSON output

type jsonStatus struct {
	Schema           int                     `json:"schema"`
	StateDir         string                  `json:"state_dir"`
	Format           int                     `json:"format"`
	FormatUnderstood bool                    `json:"format_understood"`
	FormatError      string                  `json:"format_error"`
	Gen              uint64                  `json:"gen"`
	ReconciledNS     int64                   `json:"reconciled_ns"`
	ConfigError      string                  `json:"config_error"`
	Degraded         *jsonDegraded           `json:"degraded"`
	Section          jsonSection             `json:"section"`
	Resources        map[string]jsonResource `json:"resources"`
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

type jsonSection struct {
	Held      bool    `json:"held"`
	PID       int     `json:"pid"`
	EnteredNS int64   `json:"entered_ns"`
	HeldForS  float64 `json:"held_for_s"`
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
	Stalled   bool                 `json:"stalled"`
}

func (v statusView) document() jsonStatus {
	r := v.rep
	d := jsonStatus{
		Schema: statusSchema, StateDir: r.StateDir, Format: r.Format, FormatUnderstood: r.FormatUnderstood,
		FormatError: r.FormatError, Gen: r.Gen, ReconciledNS: r.ReconciledNS, ConfigError: r.ConfigError,
		Section: jsonSection{Held: r.Section.Held, PID: r.Section.PID, EnteredNS: r.Section.EnteredNS,
			HeldForS: v.sectionFor().Seconds()},
		Resources: map[string]jsonResource{}, Holds: []jsonHold{}, Tickets: []jsonTicket{},
		DeadHolds: nonNil(r.DeadHolds), CancelRequests: nonNil(r.Cancels),
		Ignored: map[string]int{}, ObservedS: v.observed.Seconds(),
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
			need[name] = jsonRange{Min: mm[0], Max: mm[1]}
		}
		d.Tickets = append(d.Tickets, jsonTicket{ID: t.info.ID, Label: t.info.Label, PID: t.info.PID,
			Priority: t.info.Priority, Need: need, CreatedNS: t.info.CreatedNS, WaitedS: t.waited.Seconds(),
			Position: t.position, State: t.state, Stalled: t.state == "stalled"})
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

// writeJSON writes the document with labels exactly as stored: encoding/json escapes control
// characters and keeps every valid UTF-8 label decodable to the same string.
func (v statusView) writeJSON(w interface{ Write([]byte) (int, error) }) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v.document())
}
