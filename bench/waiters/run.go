//go:build coreslot_bench && unix

package main

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/core-slot/coreslot"
	"github.com/core-slot/coreslot/internal/store"
)

type kind int

const (
	kindWaiter  kind = iota
	kindControl      // a `coreslot cancel` process
	kindLeader       // the process-group leader
)

// proc is one child of the harness.
type proc struct {
	kind      kind
	serial    int
	cmd       *exec.Cmd
	pid       int
	spawned   time.Time
	enqSeq    int   // the order of its "E" line; 0 until enqueued
	baseline  int64 // its own CPU at the window's start, ns; -1 until reported
	preWindow bool  // alive when the window opened
	profiled  bool
	leaving   bool // asked to leave (cancel request or SIGTERM)
	stopped   bool
	exited    bool
	exitedAt  time.Time
	code      int // exit status, or -signal
	cpu       int64
}

type exitEvent struct {
	p    *proc
	code int
	cpu  int64
	at   time.Time
}

type lineEvent struct {
	tag string
	pid int
	val int64
}

// result is one run's measurement.
type result struct {
	Run            int     `json:"run"`
	WindowS        float64 `json:"window_s"`
	WaiterCPUS     float64 `json:"waiter_cpu_s"`
	Percent        float64 `json:"percent_of_one_core"`
	MeanWaiters    float64 `json:"mean_waiters"`
	MicrosPerWS    float64 `json:"cpu_us_per_waiter_second"`
	Arrivals       int     `json:"arrivals"`
	Admitted       int     `json:"admitted"`
	Cancelled      int     `json:"cancelled"`
	Withdrawn      int     `json:"withdrawn"`
	Stopped        int     `json:"stopped"`
	Errors         int     `json:"errors"`
	ControlCPUS    float64 `json:"control_cpu_s"`
	AllChildrenS   float64 `json:"all_children_wait4_cpu_s"`
	RusageChildren float64 `json:"rusage_children_delta_s"`
	WarmupS        float64 `json:"warmup_s"`
	GenStart       int64   `json:"gen_start"`
	GenEnd         int64   `json:"gen_end"`
	GenSeconds     int     `json:"seconds_with_gen_change"`
	StopPositions  []int   `json:"stop_positions"` // per burst: queue position of the first waiter stopped (1 = the head)
	LoadStart      string  `json:"load_start"`
	LoadEnd        string  `json:"load_end"`
}

func (r result) line() string {
	return fmt.Sprintf("run %d: %.3f %% of one core (waiters %.3f s CPU over %.1f s; mean %.0f waiters, "+
		"%.1f us CPU per waiter-second); arrivals %d, admitted %d, cancelled %d, withdrawn %d, stopped %d, "+
		"errors %d; control %.3f s; whole run: wait4 sum %.3f s, RUSAGE_CHILDREN %.3f s; warm-up %.0f s; "+
		"gen %d -> %d, changed in %d of the window's seconds; first stopped waiter's queue position per burst %v (1 = the head); load %s -> %s",
		r.Run, r.Percent, r.WaiterCPUS, r.WindowS, r.MeanWaiters, r.MicrosPerWS, r.Arrivals, r.Admitted,
		r.Cancelled, r.Withdrawn, r.Stopped, r.Errors, r.ControlCPUS, r.AllChildrenS, r.RusageChildren,
		r.WarmupS, r.GenStart, r.GenEnd, r.GenSeconds, r.StopPositions, r.LoadStart, r.LoadEnd)
}

// group is the live process group, killed by the harness's own signal handler.
var group atomic.Int64

// killGroupOnSignal makes the harness's own SIGINT, SIGTERM or SIGHUP kill the live group.
func killGroupOnSignal() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		s := <-sigs
		if g := group.Load(); g > 0 {
			_ = syscall.Kill(-int(g), syscall.SIGKILL)
		}
		fmt.Fprintf(os.Stderr, "waiters: %v: killed the process group\n", s)
		os.Exit(130)
	}()
}

// bench is the state of one run.
type bench struct {
	p      params
	run    int
	state  string
	client *coreslot.Client
	grants []*coreslot.Grant
	rng    *rand.Rand

	inR, inW, outR, outW, errFile *os.File
	pgid                          int
	env                           []string

	procs   []*proc
	byPID   map[int]*proc
	lines   chan lineEvent
	exits   chan exitEvent
	actions []action
	nextSer int
	enqN    int
	t0      time.Time
	inWin   bool
	res     result
	lastGen int64
	st      *store.Store
}

type action struct {
	at time.Time
	fn func() error
}

func runOnce(p params, run int) (result, error) {
	dir := filepath.Join(p.dir, fmt.Sprintf("run-%d-%d", run, time.Now().UnixNano()))
	failed := true
	defer func() {
		if !failed {
			_ = os.RemoveAll(dir) // the run's state directory and children's stderr; kept on failure
		}
	}()
	b := &bench{p: p, run: run, state: filepath.Join(dir, "state"), byPID: map[int]*proc{},
		lines: make(chan lineEvent, 1<<16), exits: make(chan exitEvent, 1<<14),
		rng: rand.New(rand.NewPCG(uint64(run), 18))}
	b.res.Run = run
	if err := os.MkdirAll(b.state, 0o700); err != nil {
		return result{}, err
	}
	cfg := fmt.Sprintf("[resources]\ncpu = %d\n", p.Size)
	if err := os.WriteFile(filepath.Join(b.state, "config.toml"), []byte(cfg), 0o600); err != nil {
		return result{}, err
	}
	var err error
	if b.errFile, err = os.Create(filepath.Join(dir, "children.stderr")); err != nil {
		return result{}, err
	}
	defer b.errFile.Close()
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CORESLOT_") {
			b.env = append(b.env, kv)
		}
	}
	b.env = append(b.env, "CORESLOT_STATE_DIR="+b.state)

	b.res.LoadStart = loadAvg()
	var ru0 syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &ru0); err != nil {
		return result{}, err
	}
	if b.client, err = coreslot.Open(coreslot.Options{StateDir: b.state}); err != nil {
		return result{}, err
	}
	if err := b.adoptAll(10 * time.Second); err != nil {
		return result{}, err
	}
	defer func() {
		for _, g := range b.grants {
			g.Close()
		}
	}()

	if b.inR, b.inW, err = os.Pipe(); err != nil {
		return result{}, err
	}
	if b.outR, b.outW, err = os.Pipe(); err != nil {
		return result{}, err
	}
	go b.scan(b.outR)
	defer b.outR.Close()

	leader, err := b.spawn(kindLeader, []string{"leader"})
	if err != nil {
		return result{}, err
	}
	b.pgid = leader.pid
	group.Store(int64(b.pgid))
	defer b.killAll() // every exit path: the whole group, then the pipes

	// Warm-up: N waiters, all enqueued, then a settle period.
	start := time.Now()
	for range p.N {
		if _, err := b.spawnWaiter(); err != nil {
			return result{}, err
		}
	}
	err = b.pump(start.Add(10*time.Minute), func() bool { return b.enqN >= p.N || b.res.Errors > 0 })
	if err == nil && b.res.Errors > 0 {
		err = fmt.Errorf("a waiter failed (see %s)", b.errFile.Name())
	}
	if err != nil {
		return result{}, fmt.Errorf("warm-up: %w (%d of %d enqueued)", err, b.enqN, p.N)
	}
	if err := b.pump(time.Now().Add(p.Settle), nil); err != nil {
		return result{}, err
	}
	b.res.WarmupS = time.Since(start).Seconds()

	// The window opens once every waiter has reported its own CPU so far; the CPU between a
	// waiter's report and the opening is counted, its wall time is not (a slight overestimate).
	for _, w := range b.procs {
		if w.kind == kindWaiter && !w.exited {
			w.preWindow, w.baseline = true, -1
			_ = syscall.Kill(w.pid, syscall.SIGUSR1)
		}
	}
	if err := b.pump(time.Now().Add(time.Minute), b.baselinesIn); err != nil {
		return result{}, fmt.Errorf("baselines: %w", err)
	}
	b.t0 = time.Now()
	b.inWin = true
	if b.lastGen, err = b.gen(); err != nil {
		return result{}, err
	}
	b.res.GenStart = b.lastGen
	b.at(b.t0.Add(time.Second), b.sampleGen)
	for k := 0; ; k++ {
		at := b.t0.Add(p.BurstEvery/2 + time.Duration(k)*p.BurstEvery)
		if at.After(b.t0.Add(p.Duration)) {
			break
		}
		b.at(at, b.burst)
	}
	t1 := b.t0.Add(p.Duration)
	if err := b.pump(t1, nil); err != nil {
		return result{}, err
	}

	// The window closes: the whole group dies at once and every child is reaped.
	if b.res.GenEnd, err = b.gen(); err != nil {
		return result{}, err
	}
	t1 = time.Now()
	b.inWin = false
	_ = syscall.Kill(-b.pgid, syscall.SIGKILL)
	if err := b.pump(time.Now().Add(2*time.Minute), b.allExited); err != nil {
		return result{}, fmt.Errorf("reaping: %w", err)
	}
	group.Store(0)
	var ru1 syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &ru1); err != nil {
		return result{}, err
	}
	b.res.LoadEnd = loadAvg()

	window := t1.Sub(b.t0)
	var waiterCPU, all, control int64
	var waiterSeconds float64
	for _, w := range b.procs {
		all += w.cpu
		switch w.kind {
		case kindControl:
			control += w.cpu
		case kindWaiter:
			from := w.spawned
			if w.preWindow {
				if w.baseline < 0 {
					return result{}, fmt.Errorf("waiter %d: no baseline", w.pid)
				}
				waiterCPU += w.cpu - w.baseline
				from = b.t0
			} else if w.spawned.After(b.t0) {
				waiterCPU += w.cpu
			} else {
				continue // ended before the window
			}
			until := t1
			if !w.exitedAt.IsZero() && w.exitedAt.Before(t1) {
				until = w.exitedAt
			}
			waiterSeconds += until.Sub(from).Seconds()
		}
	}
	b.res.WindowS = window.Seconds()
	b.res.WaiterCPUS = float64(waiterCPU) / 1e9
	b.res.Percent = 100 * b.res.WaiterCPUS / b.res.WindowS
	b.res.MeanWaiters = waiterSeconds / b.res.WindowS
	b.res.MicrosPerWS = float64(waiterCPU) / 1e3 / waiterSeconds
	b.res.ControlCPUS = float64(control) / 1e9
	b.res.AllChildrenS = float64(all) / 1e9
	b.res.RusageChildren = float64(rusageCPU(&ru1)-rusageCPU(&ru0)) / 1e9
	failed = false
	return b.res, nil
}

// gen reads the state directory's gen counter (one small file read, no listing or probe).
func (b *bench) gen() (int64, error) {
	if b.st == nil {
		st, err := store.Open(b.state)
		if err != nil {
			return 0, err
		}
		b.st = st
	}
	g, err := b.st.ReadGen()
	return g.N, err
}

// sampleGen reads gen once a second during the window and counts the seconds in which it
// changed: each such second makes every waiter rescan queue/ once.
func (b *bench) sampleGen() error {
	if !b.inWin {
		return nil
	}
	g, err := b.gen()
	if err != nil {
		return err
	}
	if g != b.lastGen {
		b.res.GenSeconds++
		b.lastGen = g
	}
	if next := time.Now().Add(time.Second); next.Before(b.t0.Add(b.p.Duration)) {
		b.at(next, b.sampleGen)
	}
	return nil
}

func rusageCPU(ru *syscall.Rusage) int64 { return ru.Utime.Nano() + ru.Stime.Nano() }

// killAll kills the group (a no-op once it is reaped) and closes the harness's pipe ends, so a
// child that somehow escaped the group sees its stdin close.
func (b *bench) killAll() {
	if b.pgid > 0 {
		_ = syscall.Kill(-b.pgid, syscall.SIGKILL)
	}
	group.Store(0)
	b.inW.Close()
	b.inR.Close()
	b.outW.Close()
}

func (b *bench) spawnWaiter() (*proc, error) {
	b.nextSer++
	args := []string{"waiter", "-label", "bench-" + strconv.Itoa(b.nextSer)}
	profiled := b.p.profile != "" && b.nextSer == b.p.N/2
	if profiled {
		args = append(args, "-cpuprofile", b.p.profile, "-profile-hz", strconv.Itoa(b.p.profileHz),
			"-profile-for", (b.p.Duration - 10*time.Second).String())
	}
	w, err := b.spawn(kindWaiter, args)
	if err != nil {
		return nil, err
	}
	w.serial, w.profiled = b.nextSer, profiled
	if b.inWin {
		b.res.Arrivals++
	}
	return w, nil
}

func (b *bench) spawn(k kind, args []string) (*proc, error) {
	name := b.p.self
	if k == kindControl {
		name = b.p.coreslot
	}
	cmd := exec.Command(name, args...)
	cmd.Env = b.env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = b.inR, b.outW, b.errFile
	if k == kindControl {
		cmd.Stdout = b.errFile
	}
	if k == kindLeader {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: b.pgid}
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &proc{kind: k, cmd: cmd, pid: cmd.Process.Pid, spawned: time.Now(), baseline: -1}
	b.procs = append(b.procs, p)
	b.byPID[p.pid] = p
	go func() {
		_ = cmd.Wait()
		ps := cmd.ProcessState
		ws := ps.Sys().(syscall.WaitStatus)
		code := ws.ExitStatus()
		if ws.Signaled() {
			code = -int(ws.Signal())
		}
		var cpu int64
		if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
			cpu = rusageCPU(ru)
		}
		b.exits <- exitEvent{p: p, code: code, cpu: cpu, at: time.Now()}
	}()
	return p, nil
}

// scan reads the children's shared stdout.
func (b *bench) scan(r *os.File) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 2 {
			continue
		}
		pid, _ := strconv.Atoi(f[1])
		ev := lineEvent{tag: f[0], pid: pid}
		if len(f) > 2 {
			ev.val, _ = strconv.ParseInt(f[2], 10, 64)
		}
		b.lines <- ev
	}
}

// pump handles events and scheduled actions until done reports true or deadline passes (an
// error only when done was given).
func (b *bench) pump(deadline time.Time, done func() bool) error {
	for {
		if done != nil && done() {
			return nil
		}
		next := deadline
		if len(b.actions) > 0 && b.actions[0].at.Before(next) {
			next = b.actions[0].at
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case ev := <-b.lines:
			timer.Stop()
			b.onLine(ev)
		case ev := <-b.exits:
			timer.Stop()
			b.onExit(ev)
		case <-timer.C:
			if len(b.actions) > 0 && !b.actions[0].at.After(time.Now()) {
				a := b.actions[0]
				b.actions = b.actions[1:]
				if err := a.fn(); err != nil {
					return err
				}
				continue
			}
			if !time.Now().Before(deadline) {
				if done != nil {
					return fmt.Errorf("deadline passed")
				}
				return nil
			}
		}
	}
}

// at schedules fn, keeping the actions ordered by time.
func (b *bench) at(t time.Time, fn func() error) {
	i := len(b.actions)
	for i > 0 && b.actions[i-1].at.After(t) {
		i--
	}
	b.actions = append(b.actions, action{})
	copy(b.actions[i+1:], b.actions[i:])
	b.actions[i] = action{at: t, fn: fn}
}

func (b *bench) onLine(ev lineEvent) {
	p := b.byPID[ev.pid]
	if p == nil {
		return
	}
	switch ev.tag {
	case "E":
		b.enqN++
		p.enqSeq = b.enqN
	case "B":
		p.baseline = ev.val
	}
}

func (b *bench) onExit(ev exitEvent) {
	p := ev.p
	p.exited, p.exitedAt, p.code, p.cpu = true, ev.at, ev.code, ev.cpu
	if p.kind != kindWaiter || !b.inWin {
		if p.kind == kindWaiter && ev.code == exitError {
			b.res.Errors++
		}
		return
	}
	switch ev.code {
	case exitAdmitted:
		b.res.Admitted++
	case exitCancelled:
		b.res.Cancelled++
	case exitWithdrawn:
		b.res.Withdrawn++
	default:
		b.res.Errors++
	}
}

func (b *bench) baselinesIn() bool {
	for _, w := range b.procs {
		if w.preWindow && !w.exited && w.baseline < 0 {
			return false
		}
	}
	return true
}

func (b *bench) allExited() bool {
	for _, w := range b.procs {
		if !w.exited {
			return false
		}
	}
	return true
}

// candidates are the queued waiters a burst may act on, shuffled.
func (b *bench) candidates() []*proc {
	var out []*proc
	for _, w := range b.procs {
		if w.kind == kindWaiter && w.enqSeq > 0 && !w.exited && !w.leaving && !w.stopped && !w.profiled {
			out = append(out, w)
		}
	}
	b.rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// burst is one burst: cancel requests and withdrawals, refill arrivals, released tokens for the
// admission window, then the first waiter in queue order that is a stall candidate (not
// stopped, leaving or profiled), plus Stall-1 random candidates, stopped for stall-for.
func (b *bench) burst() error {
	rep, err := b.client.Snapshot()
	if err != nil {
		return err
	}
	ids := map[int]string{}
	for _, t := range rep.Tickets {
		if !t.Dead {
			ids[t.PID] = t.ID
		}
	}
	cands := b.candidates()
	cancelled, withdrawn := 0, 0
	for _, w := range cands {
		switch {
		case cancelled < b.p.Cancel && ids[w.pid] != "":
			if _, err := b.spawn(kindControl, []string{"cancel", ids[w.pid]}); err != nil {
				return err
			}
			w.leaving = true
			cancelled++
		case withdrawn < b.p.Withdraw:
			_ = syscall.Kill(w.pid, syscall.SIGTERM)
			w.leaving = true
			withdrawn++
		}
	}
	queued := 0
	for _, w := range b.procs {
		if w.kind == kindWaiter && !w.exited && !w.leaving {
			queued++
		}
	}
	for range b.p.N - queued {
		if _, err := b.spawnWaiter(); err != nil {
			return err
		}
	}
	for i := 0; i < b.p.Release && len(b.grants) > 0; i++ {
		g := b.grants[len(b.grants)-1]
		b.grants = b.grants[:len(b.grants)-1]
		if err := g.Close(); err != nil {
			return err
		}
	}
	now := time.Now()
	b.at(now.Add(b.p.AdmitWindow), b.readopt)
	b.at(now.Add(b.p.AdmitWindow+time.Second), b.stallSome)
	return nil
}

// readopt takes the budget back, retrying every second until it holds all of it.
func (b *bench) readopt() error {
	if err := b.adoptAll(time.Second); err != nil {
		return err
	}
	if len(b.grants) < b.p.Size {
		b.at(time.Now().Add(time.Second), b.readopt)
	}
	return nil
}

func (b *bench) adoptAll(within time.Duration) error {
	deadline := time.Now().Add(within)
	for len(b.grants) < b.p.Size && time.Now().Before(deadline) {
		// Adopt only a token that is free: an adoption that comes up short publishes and removes
		// a hold, two gen bumps that make every waiter rescan, so it is not retried blindly.
		free, err := b.client.Available("cpu")
		if err != nil {
			return err
		}
		if free == 0 {
			if b.inWin {
				break // readopt tries again in a second
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		g, err := b.client.Adopt(context.Background(), coreslot.Request{Label: "bench-harness",
			Need: map[string]coreslot.Range{"cpu": {Min: 1, Max: 1}}})
		if err != nil {
			return err
		}
		if g.Shortfall()["cpu"] > 0 {
			g.Close()
			time.Sleep(20 * time.Millisecond)
			continue
		}
		b.grants = append(b.grants, g)
	}
	if len(b.grants) == 0 && b.p.Size > 0 && !b.inWin {
		return fmt.Errorf("could not adopt the budget")
	}
	return nil
}

// stallSome stops the first waiter in queue order that is a stall candidate (not stopped, leaving
// or profiled), plus Stall-1 random candidates, and continues them after stall-for. It records
// the first one's position among the live tickets of the snapshot (1 is the queue head).
func (b *bench) stallSome() error {
	cands := b.candidates()
	if len(cands) == 0 || b.p.Stall == 0 {
		return nil
	}
	rep, err := b.client.Snapshot()
	if err != nil {
		return err
	}
	byPID := make(map[int]int, len(cands))
	for i, w := range cands {
		byPID[w.pid] = i
	}
	position, live := 0, 0
	for _, tk := range rep.Tickets { // in queue order
		if tk.Dead {
			continue
		}
		live++
		if i, ok := byPID[tk.PID]; ok {
			cands[0], cands[i] = cands[i], cands[0]
			position = live
			break
		}
	}
	b.res.StopPositions = append(b.res.StopPositions, position) // 0: no candidate had a ticket
	for _, w := range cands[:min(b.p.Stall, len(cands))] {
		if err := syscall.Kill(w.pid, syscall.SIGSTOP); err != nil {
			continue
		}
		w.stopped = true
		b.res.Stopped++
		b.at(time.Now().Add(b.p.StallFor), func() error {
			if !w.exited {
				_ = syscall.Kill(w.pid, syscall.SIGCONT)
			}
			w.stopped = false
			return nil
		})
	}
	return nil
}
