//go:build unix

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/memsample"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/state"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/watch"
	"github.com/core-slot/coreslot/internal/wire"
)

// fresh reports whether a reading taken at atNS, by the wall clock, is still one at nowNS.
func fresh(atNS, nowNS int64) bool {
	return atNS > 0 && atNS <= nowNS && nowNS-atNS <= memsample.StaleNS
}

// admit is the admission attempt of the head, of a waiter that may pass it, or of a container
// request, which waits only on real memory (PROTOCOL.md section 12.1 steps 1 to 7). It returns the
// grant when admitted, nil with no error when the ticket must keep waiting (the section was busy, a
// live ticket ahead may not be passed, or the live holds leave too little), and ErrCanceled when a
// cancel request was published before this section. A gated ticket's attempt leaves the section to
// read Docker's VM and enters it again, at most three times in all (docker.go).
func (t *Ticket) admit(ctx context.Context, single bool) (*Grant, error) {
	var at attempt
	for {
		g, read, err := t.admitEntry(ctx, single, &at)
		if g != nil || err != nil || !read {
			return g, err
		}
		t.read(ctx, &at)
	}
}

// admitEntry is one section entry of an attempt; read reports that the ticket leaves to read
// Docker's VM and enters again.
func (t *Ticket) admitEntry(ctx context.Context, single bool, at *attempt) (g *Grant, read bool, err error) {
	point("admit.before-enter")
	sec, err := t.enterAdmission(ctx, single)
	if sec == nil || err != nil {
		return nil, false, err
	}
	defer sec.Leave()
	gated := t.gated()
	if !gated { // a ticket whose Docker part went ungated waits as any other
		if err := t.unnote(sec); err != nil {
			return nil, false, err
		}
	}

	// 2-4. The room: the headroom less the reserve and what the live holds still have to grow
	// into. With no hold live the head starts whatever the numbers say; a headroom that cannot be
	// read gates nothing. A container request's room leaves the holds out, except the VM growth
	// they reserve when the request is gated. The room and the level gate are the Mac test: when
	// it fails, a note goes at once.
	cfg, m, err := t.e.reading()
	if err != nil {
		return nil, false, err
	}
	holds, _, _, err := holdListing(t.e.st, spec.ColdStartMiB(m.MemMiB))
	if err != nil {
		return nil, false, err
	}
	need := t.req.NeedMiB
	var reserved []state.MemHold
	if !t.req.RealMemory || gated {
		reserved = reservations(holds)
	}
	room := state.Room(m.HeadroomMiB, cfg.ReserveMiB, reserved)
	if t.req.RealMemory && gated {
		room = state.GrowthRoom(m.HeadroomMiB, cfg.ReserveMiB, reserved)
	}
	t.out = Outcome{HeadroomMiB: m.HeadroomMiB, ReserveMiB: cfg.ReserveMiB, RoomMiB: room, HeadroomOK: m.HeadroomOK}
	if t.yield.active {
		if err := t.yieldEnds(sec, holds); err != nil {
			return nil, false, err
		}
	}
	if m.HeadroomOK && len(holds) > 0 && need > room {
		t.out.Reason, t.yield.fitSince = ReasonRoomShort, -1
		return nil, false, t.unnote(sec)
	}
	// 5. The re-election: a live ticket ahead of this one wins, unless this one may pass it. With a
	// hold live a container request needs no pass; with none the idle start is the head's.
	isHead := false
	if !t.req.RealMemory || len(holds) == 0 {
		head, headKey, headOrder, err := t.reelect()
		if err != nil {
			return nil, false, err
		}
		if head != nil && t.yield.active {
			if err := t.endYield(sec, "headship"); err != nil {
				return nil, false, err
			}
		}
		if head != nil && !t.passes(*head, headKey, headOrder, holds, room, cfg, m) {
			t.out.Reason = ReasonBehindHead
			return nil, false, nil
		}
		isHead = head == nil
	}
	var record func() // the level gate's record of this start, made only when it admits
	if len(holds) > 0 {
		wait, rec, err := t.watchWait(sec, cfg, m, need, room)
		if err != nil {
			return nil, false, err
		}
		if wait {
			return nil, false, t.unnote(sec)
		}
		record = rec
	} else {
		t.out.WatchGate = "no_holds"
	}

	// 6. A gated request's VM test, on a reading taken outside the section.
	var dock *DockerReading
	if gated {
		read, admit, err := t.vmTest(sec, holds, at)
		if read || !admit || err != nil {
			return nil, read, err
		}
		dock = &t.reading
	}
	// A head that would start now may yield to short jobs first, before the gate records its start.
	if isHead {
		if wait, err := t.yieldWait(ctx, sec, room, m); wait || err != nil {
			return nil, false, err
		}
	}
	if record != nil {
		record()
	}
	g, err = t.publishAdmission(sec, dock, false)
	return g, false, err
}

// enterAdmission enters the section for an admission and checks its format and step 1: a cancel
// request published before this section wins. It returns nil with no error when the section was
// busy; the caller leaves a section it returns.
func (t *Ticket) enterAdmission(ctx context.Context, single bool) (*store.Section, error) {
	sec, entered, err := t.e.enter(ctx, single)
	if err != nil || !entered {
		return nil, err
	}
	n, understood, err := sec.Format()
	if err == nil && !understood {
		err = refuseFormat(n, "")
	}
	cancelled := false
	if err == nil {
		cancelled, err = t.cancelRequested()
	}
	if err == nil && cancelled {
		if t.settle(statusCancelled) {
			t.removeOwn(sec)
		}
		err = ErrCanceled
	}
	if err != nil {
		sec.Leave()
		return nil, err
	}
	return sec, nil
}

// startNow admits the ticket at once on its start request (PROTOCOL.md section 12.1, the forced
// admission): no room test, level gate (so no spacing slot), order, pass or VM test. Its hold
// reserves the full need and keeps the ticket's arrival, so a red round takes it by its queue
// arrival like any other run. A gated ticket's hold carries its Docker keys from a reading taken
// outside the section, as a gated admission's does; with no reading to be had it carries none.
func (t *Ticket) startNow(ctx context.Context, single bool) (*Grant, error) {
	if t.gated() && t.e.clock.Monotonic()-t.readAt >= ReadEvery {
		t.read(ctx, &attempt{})
	}
	sec, err := t.enterAdmission(ctx, single)
	if sec == nil || err != nil {
		return nil, err
	}
	defer sec.Leave()
	t.out = Outcome{Pass: "forced"}
	var dock *DockerReading
	if t.gated() && t.reading.OK {
		dock = &t.reading
	}
	return t.publishAdmission(sec, dock, true)
}

// publishAdmission is step 7: publish the hold, close and unlink the ticket (with its note, order
// override and start request), bump gen. A Withdraw that came first wins; once the hold is
// published the ticket is admitted and Withdraw has no effect.
func (t *Ticket) publishAdmission(sec *store.Section, dock *DockerReading, forced bool) (*Grant, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != statusWaiting {
		return nil, ErrCanceled
	}
	t.out.DockerUngated, t.out.YieldEnded = t.ungated, t.yield.ended
	need := t.req.NeedMiB
	g := &Grant{e: t.e, grantMiB: need, admission: t.out, dockerUngated: t.ungated}
	if err := g.publishHold(sec, t.req, t.arrivalNS, need, dock, forced); err != nil {
		return nil, err
	}
	t.f.Close()
	_, _ = sec.RemoveOwnTicket(t.name) // already gone counts as success (PROTOCOL.md 10.3)
	if t.out.Pass == "short" {
		t.notePass(sec)
	}
	// A failed bump does not undo the admission: waiters probe the tickets ahead of them on
	// every election, so the closed ticket is passed over without a rescan.
	_ = sec.BumpGen()
	t.status, t.grant = statusAdmitted, g
	return g, nil
}

// watchWait reports whether the shared watch state keeps the ticket out while holds are live, and
// otherwise returns how to record this start for spacing, nil when it needs no record; the caller
// records it only when it admits. The detector's times are on the sample's clock, which the
// sample's age carries to now. A restart takes its part in the tick, so that calm is measured even
// when no run ticks.
func (t *Ticket) watchWait(sec *store.Section, cfg config.Config, m config.Machine, need, room int64) (bool, func(), error) {
	c := cfg.Watch
	if t.req.Restart {
		_, _, _ = sec.Tick(t.e.clock.Realtime(), t.e.sampler(cfg, m), c) // a failed sample is Unknown
	}
	d, s, at, err := t.e.st.ReadWatch()
	now := t.e.clock.Realtime()
	ok := fresh(at, now)
	mono, spacing := s.Host.WallNS+now-at, int64(c.SpacingS*1e9)
	t.out.WatchGate = watchGate(ok, d.Level)
	var refused WaitReason
	var calmNS int64
	if t.req.Restart && err == nil {
		refused, calmNS = restartRefusal(ok, d, s, at, now, c, m, need, room)
	}
	switch {
	case err != nil:
		return false, nil, err
	case refused != "":
		t.out.Reason, t.out.CalmLeftS = refused, float64(calmNS)/1e9
		return true, nil, nil
	case !ok || d.Level == watch.Unknown:
		return false, nil, nil
	case d.Level >= watch.Yellow:
		t.out.Reason = ReasonWatchLevel
		return true, nil, nil
	// Spacing needs the clear and the last start on this sample's clock: a reboot keeps watch.json.
	case s.Host.WallNS == 0 || d.YellowClearedNS > mono || d.LastAdmitNS > mono ||
		mono-d.YellowClearedNS >= int64(c.CalmS*1e9): // no clear within calm_s: no spacing
	case d.LastAdmitNS >= d.YellowClearedNS && mono-d.LastAdmitNS < spacing:
		t.out.Reason = ReasonWatchSpacing
		return true, nil, nil
	default:
		return false, func() {
			d.LastAdmitNS = mono
			_ = sec.SetWatch(d, s, at) // a start it could not record only spaces the next less
		}, nil
	}
	return false, nil, nil
}

// reelect is the re-election inside the admitting section (PROTOCOL.md section 12.1 step 5): it
// lists the names in queue/, the overrides in order/ and the notes in vmwait/ again and returns the
// body of the first live ticket ahead of this one in effective order (section 9), the head, with its
// name's key (the observer's identity) and its effective key; nil when there is none: well-formed,
// without a note, and live for this observer (liveAhead), reading the mtime (lstat) of only
// well-formed tickets.
func (t *Ticket) reelect() (*wire.Ticket, state.Key, state.Key, error) {
	ahead, err := t.listAhead()
	if err != nil {
		return nil, state.Key{}, state.Key{}, err
	}
	notes, err := t.e.notes()
	if err != nil {
		return nil, state.Key{}, state.Key{}, err
	}
	now := t.e.clock.Monotonic()
	for _, en := range ahead {
		tk, err := t.ticketBody(en.name)
		if err != nil {
			return nil, state.Key{}, state.Key{}, err
		}
		if tk == nil {
			continue
		}
		live, err := t.liveAhead(en, now)
		if err != nil {
			return nil, state.Key{}, state.Key{}, err
		}
		if live && !notes[en.name] {
			return tk, en.key, en.order, nil
		}
	}
	return nil, state.Key{}, state.Key{}, nil
}

// listAhead lists the names in queue/ and the overrides in order/ inside the section: the tickets
// ahead of this one in effective order (section 9), farthest first.
func (t *Ticket) listAhead() ([]aheadEntry, error) {
	entries, err := t.e.st.ListNames(store.QueueDir)
	if err != nil {
		return nil, err
	}
	orders, _, err := t.e.orders()
	if err != nil {
		return nil, err
	}
	t.order = orderKey(t.id, orders)
	ahead := make([]aheadEntry, 0, len(entries))
	for _, en := range entries {
		if o := orderKey(en.ID, orders); o.Less(t.order) {
			ahead = append(ahead, aheadEntry{name: en.Name, key: state.Key(en.ID), order: o})
		}
	}
	sortAhead(ahead)
	return ahead, nil
}

// liveAhead reports whether the ticket en ahead is live for this observer at now: neither gone,
// dead, stray nor STALLED, reading its mtime (lstat) and probing its lock. A ticket this observer
// has never watched, such as one published after the election, counts as live; the next election
// then rescans whatever gen says, so a ticket published without a gen bump is watched from then on
// and cannot keep the head out for longer than a stall.
func (t *Ticket) liveAhead(en aheadEntry, now time.Duration) (bool, error) {
	fi, err := lstat(filepath.Join(t.e.st.Dir(), store.QueueDir, en.name))
	if gone(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	dead, err := probe(t.e.st, store.QueueDir, en.name)
	if gone(err) || stray(err) || err == nil && dead {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, seen := t.obs.Seen(en.key); !seen {
		t.haveGen = false // rescan at the next election, so that it is watched from then on
		return true, nil
	}
	t.obs.Observe(en.key, fi.ModTime().UnixNano(), now)
	return !t.obs.Stalled(en.key, now), nil
}

// passes reports whether this ticket may start ahead of head, the first live ticket in the queue,
// whose name's key is headKey and whose effective order key is headOrder (PROTOCOL.md section
// 10.3), and records how in t.out: by duration, when its
// typical run time is known and ends within W, head's expected wait for the room it needs, at least
// the calm a restart still needs; or out of turn, whatever W is (outOfTurn).
func (t *Ticket) passes(head wire.Ticket, headKey, headOrder state.Key, holds []liveHold, room int64, cfg config.Config,
	m config.Machine) bool {
	if why, ok := t.shortPass(head, headKey, headOrder, holds, room); ok { // a yielding head fits: no other pass applies
		if why == "" {
			t.out.Pass = "short"
		}
		t.out.NoPass = why
		return why == ""
	}
	switch { // with no hold live the head starts at once
	case t.req.TypicalS == 0:
		t.out.NoPass = "no_typical"
		return false
	case len(holds) == 0:
		t.out.NoPass = "no_holds"
		return false
	}
	// W, the head's expected wait for the room it needs (headWait).
	now := t.e.clock.Realtime()
	w, noPass := headWait(head.NeedMiB, head.Restart, holds, room, now, m.HeadroomOK, t.e.st.ReadWatch, cfg.Watch)
	switch noPass {
	case "":
		t.out.W, t.out.WKnown = w, true
		if t.req.TypicalS <= w {
			t.out.Pass = "duration"
			return true
		}
		noPass = "too_long"
	case "wait_unknown":
	default: // headroom_unknown, watch_unreadable: nobody passes
		t.out.NoPass = noPass
		return false
	}
	why := t.outOfTurn(head, headKey, headOrder, holds, room, now, noPass == "wait_unknown", cfg)
	switch why {
	case "":
		t.out.Pass = "turn"
		return true
	case "too_long": // too long for both: say what W said, too_long or wait_unknown
		why = noPass
	}
	t.out.NoPass = why
	return false
}

// pressureNormal is the kernel's normal memory pressure level, as memsample.Parts reports it.
const pressureNormal = 1

// kernelPressure is the kernel's memory pressure level and whether it could be read; a system that
// has none (Linux) reports it as not read. Tests replace it.
var kernelPressure = memsample.Pressure

// outOfTurn is the first condition that keeps this ticket from going out of turn ahead of head, ""
// when none does (PROTOCOL.md section 10.3): head does not fit the room now; the kernel's memory
// pressure level is normal (one that cannot be read is not); every live hold's body parses and none
// arrived after head's effective order key (or in the same nanosecond), so no other run is out of
// turn; and the ticket's typical run
// time is at most the time it has watched head, its own absences excluded, or, when head's W is
// unknown and head is a restart, the calm head still needs.
func (t *Ticket) outOfTurn(head wire.Ticket, headKey, headOrder state.Key, holds []liveHold, room, nowNS int64,
	wUnknown bool, cfg config.Config) string {
	if head.NeedMiB <= room {
		return "head_fits"
	}
	if level, ok := kernelPressure(); !ok || level != pressureNormal {
		return "pressure_not_normal"
	}
	for _, h := range holds {
		if h.unparsed {
			return "hold_unreadable"
		}
	}
	for _, h := range holds {
		if uint64(h.body.ArrivalNS) >= headOrder.OrderKey { // the same nanosecond counts as after
			return "out_of_turn_live"
		}
	}
	var calm int64 // with W known, W already holds the calm and a longer run passes on watched time alone
	if wUnknown {
		var noPass string
		if calm, noPass = restartFloor(head.Restart, nowNS, t.e.st.ReadWatch, cfg.Watch); noPass != "" {
			return noPass
		}
	}
	watched := int64(t.obs.WatchedFor(headKey, t.e.clock.Monotonic()) / time.Second)
	if t.req.TypicalS > max(watched, calm) {
		return "too_long"
	}
	return ""
}

// calmLeft is how much of calm_s, in ns, a restart still needs at now: what the sample's green
// stretch, carried to now by the sample's age, has not covered yet; all of it without a fresh
// green sample.
func calmLeft(d watch.Detector, s watch.Sample, at, now int64, c watch.Config) int64 {
	calm := int64(c.CalmS * 1e9)
	if fresh(at, now) && d.Level == watch.Green {
		calm -= s.Host.WallNS + now - at - d.CalmSinceNS
	}
	return calm
}

// Grant is what an admission holds: the hold's lock-carrying descriptor, which carries the
// reservation in its body. Its methods are safe for concurrent use.
type Grant struct {
	e         *Engine
	hold      *os.File
	holdName  string
	grantMiB  int64   // the need the command was granted
	failOpen  string  // the reason of a fail-open run's grant
	admission Outcome // the attempt that admitted it; never changes

	dockerGated   bool
	dockerVMMiB   int64
	dockerUngated string

	mu     sync.Mutex
	body   wire.Hold // the hold's body as last written
	closed bool
}

// publishHold publishes the grant's hold for req, which arrived at arrivalNS, reserving needMiB,
// inside sec and keeps its descriptor. A grant admitted on its Docker need by the reading dock
// carries that need, the VM growth it reserves on the host and the reading's stamp from this first
// body on.
func (g *Grant) publishHold(sec *store.Section, req spec.Request, arrivalNS, needMiB int64, dock *DockerReading, forced bool) error {
	start := max(g.e.clock.Realtime(), 0)
	h := wire.Hold{Label: req.Label, PID: os.Getpid(), StartNS: start, ArrivalNS: max(arrivalNS, 0),
		NeedMiB: needMiB, TypicalS: req.TypicalS, Forced: forced}
	if dock != nil {
		h.DockerNeedMiB, h.DockerSampledNS = req.DockerNeedMiB, max(dock.StartedNS, 0)
		h.DockerGrowthMiB = min(state.Growth(dock.VMMiB, dock.VMNowMiB, req.DockerNeedMiB, req.DockerFootprintMiB), spec.MaxNeedMiB)
		g.dockerGated, g.dockerVMMiB = true, dock.VMMiB
	}
	f, name, err := sec.PublishHold(h, g.e.seq())
	if err != nil {
		return err
	}
	g.hold, g.holdName, g.body = f, name, h
	return nil
}

// GrantMiB is the memory the command was granted, in MiB: the need.
func (g *Grant) GrantMiB() int64 { return g.grantMiB }

// ArrivalNS is the wall clock at the run's first arrival in the queue, as its hold carries it.
func (g *Grant) ArrivalNS() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.body.ArrivalNS
}

// HoldName is the hold's file name in holds/; empty without a hold.
func (g *Grant) HoldName() string { return g.holdName }

// Files are the descriptors for a child, for exec.Cmd.ExtraFiles: the hold (fd 3), nil when there
// is none so that fd 3 stays closed (PROTOCOL.md section 17).
func (g *Grant) Files() []*os.File { return []*os.File{g.hold} }

// Env is the CORESLOT_* environment matching Files; the caller removes every inherited context
// variable (IsContext) before appending it (PROTOCOL.md section 17).
func (g *Grant) Env() []string { return ChildEnv(g.grantMiB, g.holdName, g.failOpen) }

// UpdateHold rewrites the grant's hold body with what fn changes in it, in place and without the
// section (store.RewriteHold); the label, pid and start time the name carries stay. It fails once
// the grant is closed or when it has no hold.
func (g *Grant) UpdateHold(fn func(*wire.Hold)) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.hold == nil {
		return errors.New("engine: the grant holds no hold to update")
	}
	h := g.body
	fn(&h)
	h.Label, h.PID, h.StartNS = g.body.Label, g.body.PID, g.body.StartNS
	if err := store.RewriteHold(g.hold, filepath.Join(g.e.st.Dir(), store.HoldsDir, g.holdName), h); err != nil {
		return err
	}
	g.body = h
	return nil
}

// Close closes this process's copy of the hold's descriptor; it never unlocks, so a child that
// inherited a copy keeps the grant until it exits. Then, if it can enter the section within 2 s,
// it probes the hold and removes it only when the probe succeeds (PROTOCOL.md section 12.3);
// otherwise reconciliation removes it later.
//
// Before closing it checks I1 for the hold: it must still be linked and its path must name the
// same file. When it is not (the state directory was removed, or removed and recreated, under the
// holder), the holder reports it and writes nothing into the directory as it now exists: it
// closes the descriptor, removes no hold, enters no section, and returns the mismatch as an
// *IdentityError around a *store.StateError. It is idempotent and otherwise returns the error
// from closing the descriptor.
func (g *Grant) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.mu.Unlock()
	if g.hold == nil {
		return nil
	}
	identity := store.CheckHeld(g.hold, filepath.Join(g.e.st.Dir(), store.HoldsDir, g.holdName))
	err := g.hold.Close()
	if identity != nil {
		return &IdentityError{Err: identity}
	}
	g.removeHold()
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

// removeHold is the best-effort removal of a closed grant's hold.
func (g *Grant) removeHold() {
	ctx, cancel := context.WithTimeout(context.Background(), releaseWait)
	defer cancel()
	sec, err := enterSection(g.e.st, ctx)
	if err != nil {
		return
	}
	defer sec.Leave()
	if _, understood, err := sec.Prepare(g.e.clock.Realtime()); err != nil || !understood {
		return
	}
	if removed, err := sec.ProbeAndRemoveHold(g.holdName); err == nil && removed {
		_ = sec.BumpGen() // removing one's own hold bumps gen once (PROTOCOL.md section 7.8)
	}
}
