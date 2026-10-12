//go:build coreslot_testhooks && unix

// Package conformance drives real coreslot processes through the traces of spec section 10.1:
// the CLI binary and the helper in conformance/child, both built with the coreslot_testhooks
// tag, against temporary state directories whose paths contain a space. A trace pauses a
// process at a named test-hook point, stops, continues or kills it, and reads the state
// directory through the library's Snapshot.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/testhooks"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// Bounds of the harness itself, generous for a heavily loaded host. The traces' own bounds are
// stated in the traces.
const (
	hookWait  = 90 * time.Second // a process reaching a pause point
	startWait = 60 * time.Second // a run's command starting, a ticket or hold appearing
	exitWait  = 60 * time.Second // a process exiting once it may
	pollStep  = 20 * time.Millisecond
)

// ---------------------------------------------------------------- binaries

var (
	buildOnce sync.Once
	buildDir  string // created by TestMain, removed after the run
	buildErr  error
	bins      struct{ coreslot, child string }
)

// Build builds the coreslot CLI and the helper child with the coreslot_testhooks tag, once per
// test binary, and returns the CLI's path.
func Build(t *testing.T) (bin string) {
	t.Helper()
	buildOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			buildErr = err
			return
		}
		for _, b := range []struct {
			out       *string
			pkg, name string
		}{
			{&bins.coreslot, "./cmd/coreslot", "coreslot"},
			{&bins.child, "./conformance/child", "child"},
		} {
			out := filepath.Join(buildDir, b.name)
			cmd := exec.Command("go", "build", "-tags", "coreslot_testhooks", "-o", out, b.pkg)
			cmd.Dir = root
			if msg, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("go build %s: %v\n%s", b.pkg, err, msg)
				return
			}
			*b.out = out
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return bins.coreslot
}

func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %v", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == os.DevNull {
		return "", errors.New("not inside the module")
	}
	return filepath.Dir(mod), nil
}

// ---------------------------------------------------------------- process registry

// Every process the harness starts is registered, so that a test that fails, and a test binary
// about to hit its -timeout, kill them all with their process groups.
var registry struct {
	sync.Mutex
	procs map[*Proc]bool
}

func register(p *Proc) {
	registry.Lock()
	defer registry.Unlock()
	if registry.procs == nil {
		registry.procs = map[*Proc]bool{}
	}
	registry.procs[p] = true
}

func unregister(p *Proc) {
	registry.Lock()
	defer registry.Unlock()
	delete(registry.procs, p)
}

func killAll() {
	registry.Lock()
	procs := make([]*Proc, 0, len(registry.procs))
	for p := range registry.procs {
		procs = append(procs, p)
	}
	registry.Unlock()
	for _, p := range procs {
		p.kill(-p.pid, syscall.SIGKILL)
	}
}

func TestMain(m *testing.M) {
	flag.Parse()
	dir, err := os.MkdirTemp("", "coreslot-conformance-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	buildDir = dir
	// A watchdog kills every started process group shortly before -timeout panics the binary,
	// when no deferred cleanup would run.
	if f := flag.Lookup("test.timeout"); f != nil {
		if d, err := time.ParseDuration(f.Value.String()); err == nil && d > 0 {
			margin := min(d/10, 30*time.Second)
			time.AfterFunc(d-margin, func() {
				fmt.Fprintln(os.Stderr, "conformance: -timeout is near; killing every started process group")
				killAll()
			})
		}
	}
	code := m.Run()
	killAll()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ---------------------------------------------------------------- environment

// Env is one state directory and its surroundings: a hook directory for pause handshakes, a
// marker directory where helper children report, and the settings the next Start passes on.
type Env struct {
	t       *testing.T
	Dir     string // the state directory; its path contains a space
	HookDir string
	MarkDir string
	OutDir  string // the processes' stdout and stderr files
	shared  *shared

	pause  []string      // CORESLOT_TESTHOOK_PAUSE of the next Start
	offset time.Duration // CORESLOT_TESTHOOK_REALTIME_OFFSET of the next Start
}

type shared struct {
	once   sync.Once
	client *coreslot.Client
	err    error
}

// State returns a fresh environment. The state directory itself does not exist yet, as on a
// machine coreslot never ran on; its path contains a space, as macOS's default does.
func State(t *testing.T) Env {
	t.Helper()
	Build(t)
	base := t.TempDir()
	e := Env{t: t, Dir: filepath.Join(base, "Application Support", "coreslot"),
		HookDir: filepath.Join(base, "hooks"), MarkDir: filepath.Join(base, "marks"), OutDir: filepath.Join(base, "out"),
		shared: &shared{}}
	for _, d := range []string{e.HookDir, e.MarkDir, e.OutDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// WriteConfig writes body as the state directory's config.toml (PROTOCOL.md section 5.3: under
// the override the configuration lives there), creating the directory when needed.
func (e Env) WriteConfig(body string) {
	e.t.Helper()
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Dir, store.ConfigFile), []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// on returns e reporting to t, for a subtest.
func (e Env) on(t *testing.T) Env {
	e.t = t
	return e
}

// SectionLeft waits until the admission section is observed free. A ticket or hold becomes
// visible inside the section that publishes it, before that section bumps gen and leaves; once
// the section is seen free, every section that published what the trace has observed so far
// has ended, its gen bump included. Call it before stopping a process whose publication was
// just observed, or before reading a gen baseline.
func (e Env) SectionLeft() {
	e.t.Helper()
	e.Eventually(startWait, "the admission section is observed free", func() bool { return !e.Status().Section.Held })
}

// Pausing returns e whose next Start pauses at points.
func (e Env) Pausing(points ...string) Env {
	e.pause = points
	return e
}

// Offset returns e whose next Start runs with its wall clock shifted by d.
func (e Env) Offset(d time.Duration) Env {
	e.offset = d
	return e
}

// StepClock sets the wall-clock offset of every process sharing e's hook directory to d, at
// once and while they run: it replaces the hook directory's realtime-offset file through a
// rename, which each process re-reads on its next wall-clock read. The file overrides the offset
// a process was started with (Offset).
func (e Env) StepClock(d time.Duration) {
	e.t.Helper()
	path := filepath.Join(e.HookDir, testhooks.OffsetFile)
	if err := os.WriteFile(path+".partial", []byte(d.String()+"\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Rename(path+".partial", path); err != nil {
		e.t.Fatal(err)
	}
}

func (e Env) environ() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CORESLOT_") {
			env = append(env, kv)
		}
	}
	env = append(env, "CORESLOT_STATE_DIR="+e.Dir, "CORESLOT_TESTHOOK_DIR="+e.HookDir)
	if len(e.pause) > 0 {
		env = append(env, "CORESLOT_TESTHOOK_PAUSE="+strings.Join(e.pause, ","))
	}
	if e.offset != 0 {
		env = append(env, "CORESLOT_TESTHOOK_REALTIME_OFFSET="+e.offset.String())
	}
	return env
}

// Start starts the coreslot CLI with args.
func (e Env) Start(args ...string) *Proc {
	e.t.Helper()
	return e.StartCommand(append([]string{bins.coreslot}, args...)...)
}

// StartChild starts the helper child directly with args.
func (e Env) StartChild(args ...string) *Proc {
	e.t.Helper()
	return e.StartCommand(append([]string{bins.child}, args...)...)
}

// StartCommand starts argv in its own process group with e's environment, its output going
// to files. The process is killed, with its group, when the test ends.
//
// The harness reaps the process itself (wait4 with WNOHANG, under the Proc's mutex), and every
// signal is sent under that mutex only while the process is not reaped: until then its pid,
// which is also its process group id, cannot be reused, so a signal never reaches a stranger.
func (e Env) StartCommand(argv ...string) *Proc {
	e.t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = e.environ()
	return e.startCmd(cmd, nil)
}

// startCmd starts cmd as StartCommand describes: its own process group, its output to files,
// reaped and killed with its group by the harness. With a non-nil tty the process instead
// leads a new session whose controlling terminal, stdin, stdout and stderr are tty, and its
// output is read from the terminal.
func (e Env) startCmd(cmd *exec.Cmd, tty *os.File) *Proc {
	e.t.Helper()
	p := &Proc{t: e.t, env: e, done: make(chan struct{}), name: filepath.Base(cmd.Path)}
	var err error
	if p.stdout, err = os.CreateTemp(e.OutDir, "stdout-"); err != nil {
		e.t.Fatal(err)
	}
	if p.stderr, err = os.CreateTemp(e.OutDir, "stderr-"); err != nil {
		p.stdout.Close()
		e.t.Fatal(err)
	}
	if tty != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	} else {
		cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		p.stdout.Close()
		p.stderr.Close()
		e.t.Fatalf("start %v: %v", cmd.Args, err)
	}
	p.pid = cmd.Process.Pid
	register(p)
	go p.reap(cmd.Process)
	e.t.Cleanup(func() {
		// Whatever a run started in another group (its command) dies with its run: the helper
		// child exits when it is reparented.
		if p.kill(-p.pid, syscall.SIGKILL) {
			select {
			case <-p.done:
			case <-time.After(exitWait):
				e.t.Errorf("pid %d did not exit after SIGKILL", p.pid)
			}
		}
		p.stdout.Close()
		p.stderr.Close()
	})
	return p
}

// reap waits for the process to exit and reaps it, holding p.mu while it reaps.
func (p *Proc) reap(proc *os.Process) {
	defer close(p.done)
	defer unregister(p)
	for {
		p.mu.Lock()
		var ws unix.WaitStatus
		wpid, err := unix.Wait4(p.pid, &ws, unix.WNOHANG, nil)
		if wpid == p.pid || (err != nil && !errors.Is(err, unix.EINTR)) {
			p.reaped = true
			p.code = -1
			if err == nil && ws.Exited() {
				p.code = ws.ExitStatus()
			}
			p.mu.Unlock()
			_ = proc.Release()
			return
		}
		p.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
}

// kill sends sig to target (the pid, or the process group as -pid) while the process is not
// reaped; it reports whether it signalled.
func (p *Proc) kill(target int, sig syscall.Signal) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped {
		return false
	}
	_ = syscall.Kill(target, sig)
	return true
}

// Run starts `coreslot run` of a helper child that reports and holds until ReleaseChild(name):
// the label is name and flags go before `--`.
func (e Env) Run(name string, flags ...string) *Proc {
	e.t.Helper()
	args := append([]string{"run", "--label", name}, flags...)
	args = append(args, "--", bins.child, "hold", e.MarkDir, name)
	p := e.Start(args...)
	p.name = name
	return p
}

// ChildReport is what a helper child started by a run reports.
type ChildReport struct {
	PID  int               `json:"pid"`
	Env  map[string]string `json:"env"`
	FDs  map[int][2]uint64 `json:"fds"`
	Args []string          `json:"args"`
}

// Started returns the report of the child of the run named name, if it started.
func (e Env) Started(name string) (ChildReport, bool) {
	data, err := os.ReadFile(filepath.Join(e.MarkDir, name+".started"))
	if err != nil {
		return ChildReport{}, false
	}
	var r ChildReport
	if err := json.Unmarshal(data, &r); err != nil {
		e.t.Fatalf("child report %s: %v", name, err)
	}
	return r, true
}

// WaitStarted waits until the child of the run named name started.
func (e Env) WaitStarted(name string) ChildReport {
	e.t.Helper()
	var r ChildReport
	e.Eventually(startWait, "the command of "+name+" starts", func() bool {
		var ok bool
		r, ok = e.Started(name)
		return ok
	})
	return r
}

// ReleaseChild lets the child of the run named name exit 0.
func (e Env) ReleaseChild(name string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.MarkDir, name+".release"), nil, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// Client is the test process's own library client of the state directory.
func (e Env) Client() *coreslot.Client {
	e.t.Helper()
	e.shared.once.Do(func() {
		e.shared.client, e.shared.err = coreslot.Open(coreslot.Options{StateDir: e.Dir})
	})
	if e.shared.err != nil {
		e.t.Fatalf("open %s: %v", e.Dir, e.shared.err)
	}
	return e.shared.client
}

// Report is the library's snapshot of the state directory.
type Report = coreslot.Report

// Status reads the state directory through the library's Snapshot. Snapshot reads outside the
// section and only probes admit.lock with one non-blocking attempt to enter (left at once when
// it succeeds), so it never waits and works while a process is stopped inside the section; the
// probe can make another client's single attempt find the section busy for that instant.
func (e Env) Status() Report {
	e.t.Helper()
	r, err := e.Client().Snapshot()
	if err != nil {
		e.t.Fatalf("snapshot: %v", err)
	}
	return r
}

// Ticket returns the queue entry labelled label.
func (e Env) Ticket(label string) (coreslot.TicketInfo, bool) {
	e.t.Helper()
	for _, tk := range e.Status().Tickets {
		if tk.Label == label {
			return tk, true
		}
	}
	return coreslot.TicketInfo{}, false
}

// Hold returns the live hold labelled label.
func (e Env) Hold(label string) (coreslot.HolderInfo, bool) {
	e.t.Helper()
	for _, h := range e.Status().Holds {
		if h.Label == label {
			return h, true
		}
	}
	return coreslot.HolderInfo{}, false
}

// WaitTicket waits until a live ticket labelled label is in the queue.
func (e Env) WaitTicket(label string) coreslot.TicketInfo {
	e.t.Helper()
	var tk coreslot.TicketInfo
	e.Eventually(startWait, "the ticket of "+label+" is queued", func() bool {
		var ok bool
		tk, ok = e.Ticket(label)
		return ok && !tk.Dead
	})
	return tk
}

// WaitHold waits until a live hold labelled label exists.
func (e Env) WaitHold(label string, within time.Duration) coreslot.HolderInfo {
	e.t.Helper()
	var h coreslot.HolderInfo
	e.Eventually(within, "the hold of "+label+" is published", func() bool {
		var ok bool
		h, ok = e.Hold(label)
		return ok
	})
	return h
}

// Eventually polls cond every 20 ms until it holds, failing the test after within.
func (e Env) Eventually(within time.Duration, what string, cond func() bool) {
	e.t.Helper()
	end := time.Now().Add(within)
	for !cond() {
		if time.Now().After(end) {
			e.t.Fatalf("%s: not within %v (load: %s)", what, within, uptime())
		}
		time.Sleep(pollStep)
	}
}

// Tmps lists the temporary files of pid in the layout directory dir ("" for the state
// directory itself).
func (e Env) Tmps(dir string, pid int) []string {
	names, _ := filepath.Glob(filepath.Join(e.Dir, dir, fmt.Sprintf(".tmp-%010d-*", pid)))
	return names
}

// AllTmps lists every temporary file of the layout.
func (e Env) AllTmps() []string {
	var all []string
	for _, d := range []string{"", store.QueueDir, store.HoldsDir, store.CancelDir} {
		names, _ := filepath.Glob(filepath.Join(e.Dir, d, ".tmp-*"))
		all = append(all, names...)
	}
	return all
}

// Reconcile forces a reconciliation now: the helper enters the section and reconciles as a
// client whose clock is 60 s ahead, so that it is due and every temporary file is old enough.
// It returns how many entries it removed.
func (e Env) Reconcile() int {
	e.t.Helper()
	p := e.Pausing().StartChild("reconcile", e.Dir, "60s")
	if code := p.Wait(); code != 0 {
		e.t.Fatalf("forced reconciliation exited %d: %s", code, p.Stderr())
	}
	var n int
	if _, err := fmt.Sscanf(p.Stdout(), "removed %d", &n); err != nil {
		e.t.Fatalf("forced reconciliation printed %q", p.Stdout())
	}
	return n
}

// HoldBody reads the body of the hold named name.
func (e Env) HoldBody(name string) wire.Hold {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.Dir, store.HoldsDir, name))
	if err != nil {
		e.t.Fatal(err)
	}
	h, err := wire.DecodeHold(data)
	if err != nil {
		e.t.Fatalf("hold %s: %v", name, err)
	}
	return h
}

// ---------------------------------------------------------------- processes

// Proc is a started process.
type Proc struct {
	t    *testing.T
	env  Env
	pid  int
	name string

	stdout, stderr *os.File

	mu     sync.Mutex // held while reaping and while signalling
	reaped bool
	done   chan struct{}
	code   int
}

// PID is the process id.
func (p *Proc) PID() int { return p.pid }

// Stdout and Stderr are what the process wrote so far.
func (p *Proc) Stdout() string { return readAll(p.stdout) }
func (p *Proc) Stderr() string { return readAll(p.stderr) }

func readAll(f *os.File) string {
	data, _ := os.ReadFile(f.Name())
	return string(data)
}

func (p *Proc) hookPath(point, suffix string) string {
	return filepath.Join(p.env.HookDir, fmt.Sprintf("%d.%s.%s", p.pid, point, suffix))
}

// paused reports that p waits at point and has not been released: the .reached file exists
// and the .go file does not (Point removes .reached before .go, so this never matches an
// occurrence that is already released).
func (p *Proc) paused(point string) bool {
	if _, err := os.Stat(p.hookPath(point, "reached")); err != nil {
		return false
	}
	_, err := os.Stat(p.hookPath(point, "go"))
	return errors.Is(err, os.ErrNotExist)
}

// WaitReached waits until p is paused at point.
func (p *Proc) WaitReached(point string) {
	p.t.Helper()
	end := time.Now().Add(hookWait)
	for !p.paused(point) {
		if p.Exited() {
			p.t.Fatalf("%s (pid %d) exited %d before reaching %s; stderr:\n%s", p.name, p.pid, p.code, point, p.Stderr())
		}
		if time.Now().After(end) {
			p.t.Fatalf("%s (pid %d) did not reach %s within %v (load: %s); stderr:\n%s",
				p.name, p.pid, point, hookWait, uptime(), p.Stderr())
		}
		time.Sleep(pollStep)
	}
}

// Release lets p go on from point. It does not wait: p may be stopped.
func (p *Proc) Release(point string) {
	p.t.Helper()
	if err := os.WriteFile(p.hookPath(point, "go"), nil, 0o600); err != nil {
		p.t.Fatal(err)
	}
}

// AutoRelease releases every occurrence of point from now on, until the returned function is
// called; that function returns once no further release can happen.
func (p *Proc) AutoRelease(point string) (stop func()) {
	quit, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-quit:
				return
			case <-p.done:
				return
			case <-time.After(pollStep):
			}
			if p.paused(point) {
				_ = os.WriteFile(p.hookPath(point, "go"), nil, 0o600)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(quit); <-finished }) }
}

// CatchPublication waits until p is paused at point while it publishes into the layout
// directory dir (its temporary file is there), releasing every earlier occurrence of point.
func (p *Proc) CatchPublication(point, dir string) {
	p.t.Helper()
	for {
		p.WaitReached(point)
		if len(p.env.Tmps(dir, p.pid)) > 0 {
			return
		}
		p.Release(point)
		p.env.Eventually(hookWait, fmt.Sprintf("pid %d consumes the release of %s", p.pid, point), func() bool {
			_, err := os.Stat(p.hookPath(point, "go"))
			return errors.Is(err, os.ErrNotExist) || p.Exited()
		})
	}
}

// Stop, Cont and Kill signal the process: SIGSTOP and SIGCONT to it alone, SIGKILL to its
// whole process group.
func (p *Proc) Stop() { p.signal(p.pid, syscall.SIGSTOP) }
func (p *Proc) Cont() { p.signal(p.pid, syscall.SIGCONT) }
func (p *Proc) Kill() {
	p.signal(-p.pid, syscall.SIGKILL)
	p.Wait()
}

func (p *Proc) signal(target int, sig syscall.Signal) {
	p.t.Helper()
	if !p.kill(target, sig) {
		p.t.Fatalf("%s (pid %d) already exited (%d) before %v; stderr:\n%s", p.name, p.pid, p.code, sig, p.Stderr())
	}
}

// Exited reports whether the process has exited.
func (p *Proc) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Wait waits for the process to exit and returns its exit status (-1 when a signal killed it).
func (p *Proc) Wait() int {
	p.t.Helper()
	select {
	case <-p.done:
		return p.code
	case <-time.After(exitWait):
		p.t.Fatalf("%s (pid %d) did not exit within %v (load: %s); stderr:\n%s", p.name, p.pid, exitWait, uptime(), p.Stderr())
		return 0
	}
}

// ExitCode waits for the process and checks its exit status.
func (p *Proc) ExitCode(want int) {
	p.t.Helper()
	if got := p.Wait(); got != want {
		p.t.Fatalf("%s (pid %d) exited %d, want %d; stderr:\n%s", p.name, p.pid, got, want, p.Stderr())
	}
}

// uptime is the host's load, recorded beside every timing failure.
func uptime() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "uptime").Output()
	if err != nil {
		return fmt.Sprintf("uptime unavailable (%v; %d CPUs)", err, runtime.NumCPU())
	}
	return strings.TrimSpace(string(out))
}
