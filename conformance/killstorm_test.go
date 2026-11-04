//go:build coreslot_testhooks && unix

package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/store"
)

// The kill-storm: randomized CLI clients, some of them nested runs, queue for mem while the storm kills them with SIGKILL, stops and continues them, cancels them, and
// shrinks and grows the room through the configuration. A sampler checks I1, I2 and I3 on the hold
// bodies throughout, and every client the storm did not kill must end as its fate says: no run
// killed a command other than its own (the kill side of I7). Every run prints its seed; CORESLOT_STORM_SEED=<seed> replays the same schedule (each client's kind, request, fate
// and delays, and the room changes; the interleaving is the scheduler's). The schedule also
// depends on -short. The first clients and the first two room changes are fixed, and one
// client is canceled while it is seen queued, so that every run exercises each kind of
// client, a kill of a holder, a stop, a cancellation, a shrink below what is reserved and a
// growth; the test asserts that it did.
//
// Mutation: dropping the holds from state.Room admits the memFlood clients past the room the
// first wave leaves (I3).

const (
	stormFull, stormShort     = 60 * time.Second, 10 * time.Second
	clientsFull, clientsShort = 50, 20
	stormSeedEnv              = "CORESLOT_STORM_SEED"
	// windDown bounds the end of the storm: every client killed and reaped.
	windDown = 60 * time.Second
	// The rooms the configuration leaves after the first two changes range over
	// stormRoomMin..stormRoomMax MiB.
	stormRoomMin, stormRoomMax = 1792, 2560
)

// stormLabel is s<id>.<kind>. It uses only characters a hold's display keeps, so the display of
// a ticket's name is the label itself.
func stormLabel(id int, kind string) string {
	return fmt.Sprintf("s%05d.%s", id, kind)
}

// ---------------------------------------------------------------- reports

// readReport reads the report of the command named name, without failing the test: it is
// called off the test goroutine.
func readReport(markDir, name string) (ChildReport, bool) {
	data, err := os.ReadFile(filepath.Join(markDir, name+".started"))
	if err != nil {
		return ChildReport{}, false
	}
	var r ChildReport
	if json.Unmarshal(data, &r) != nil {
		return ChildReport{}, false
	}
	return r, true
}

// ---------------------------------------------------------------- the storm

type stormClient struct {
	id          int
	kind        string // cli, or cli-go for a nested run
	inner       string // "" or go: the nested run
	name        string // the label, and the marker name of its command
	need        int    // MiB
	timeout     time.Duration
	fate        string // release, kill (at a time), killheld (once it holds), killheld-after-shrink, stop or cancel
	at          time.Duration
	stopFor     time.Duration
	hold        time.Duration
	afterShrink bool // a first-wave client: holds until the first shrink

	proc      *Proc
	killed    bool // the storm sent it SIGKILL
	stopped   atomic.Bool
	cancelled atomic.Bool // the canceller published a cancel request for it
}

type stormResult struct {
	c        *stormClient
	code     int
	stderr   string
	admitted bool
}

type storm struct {
	t       *testing.T
	e       Env
	s       *Sampler
	seed    uint64
	short   bool
	started time.Time
	shrunk  chan struct{} // closed after the first shrink, or when the storm ends without it

	mu      sync.Mutex
	nextID  int
	results []stormResult
	helpers []*Proc // cancel commands
	live    map[*stormClient]bool
	victim  string // the kind of the first-wave client killed while it holds
	counts  map[string]int
}

func (st *storm) count(what string) {
	st.mu.Lock()
	st.counts[what]++
	st.mu.Unlock()
}

// firstWave is the number of fixed clients at the start of every run, and memFlood the number of
// fixed mem clients after them.
const firstWave, memFlood = 4, 3

// victimStream is the stream of the seed that picks the first-wave client killed while it
// holds; client ids and the room changes (stream 0) use the others.
const victimStream = 1<<64 - 1

// victimID is the first-wave client this run kills while it holds, chosen by the seed.
func (st *storm) victimID() int {
	return 1 + rand.New(rand.NewPCG(st.seed, victimStream)).IntN(firstWave)
}

// plan draws client id's schedule from its own stream of the seed, so that a client's plan does
// not depend on the order in which the slots ask for theirs. Clients 1 to firstWave are fixed:
// four CLI admissions, the third with a nested run, each holding mem until the first shrink:
// 128 MiB each, but 768 for the first and the third, so the first wave holds 1792, all of the
// room. Then each lets go, except the one victimID picks, which is killed while it holds. The
// memFlood clients after them are CLI runs of 1024 MiB and nothing else, each holding for a
// second, which arrive while the first wave holds and find nothing ahead of them: the room the
// holds leave admits none beside the first wave, while a room that forgets the holds admits all
// three.
// Client firstWave+memFlood+1 is stopped and continued.
func (st *storm) plan(id int) *stormClient {
	r := rand.New(rand.NewPCG(st.seed, uint64(id)))
	c := &stormClient{id: id}
	if r.IntN(6) == 0 {
		c.inner = "go"
	}
	c.need = 128 + r.IntN(897)
	if r.IntN(5) == 0 {
		c.timeout = time.Second + time.Duration(r.IntN(5000))*time.Millisecond
	}
	switch x := r.IntN(100); {
	case x < 45:
		c.fate = "release"
	case x < 57:
		c.fate = "kill"
	case x < 65:
		c.fate = "killheld"
	case x < 82:
		c.fate = "stop"
	default:
		c.fate = "cancel"
	}
	c.at = time.Duration(r.IntN(4000)) * time.Millisecond
	c.stopFor = 100*time.Millisecond + time.Duration(r.IntN(2900))*time.Millisecond
	if !st.short && r.IntN(12) == 0 {
		c.stopFor = 10500*time.Millisecond + time.Duration(r.IntN(2500))*time.Millisecond // STALLED
	}
	c.hold = 50*time.Millisecond + time.Duration(r.IntN(1950))*time.Millisecond

	if id <= firstWave {
		c.inner = ""
		if id == 3 {
			c.inner = "go"
		}
		c.need = 128
		if id == 1 || id == 3 {
			c.need = 768
		}
		c.timeout, c.fate, c.afterShrink = 0, "release", true
		if id == st.victimID() {
			c.fate = "killheld-after-shrink"
		}
	}
	if id > firstWave && id <= firstWave+memFlood {
		c.inner = ""
		c.need = 1024
		c.timeout, c.fate, c.hold = 0, "release", time.Second
	}
	if id == firstWave+memFlood+1 {
		c.fate, c.at, c.stopFor = "stop", 500*time.Millisecond, 1500*time.Millisecond
	}
	c.kind = "cli"
	if c.inner != "" {
		c.kind += "-" + c.inner
	}
	c.name = stormLabel(id, c.kind)
	return c
}

func (st *storm) argv(c *stormClient) []string {
	leaf := []string{bins.child, "hold", st.e.MarkDir, c.name}
	cmd := leaf
	if c.inner == "go" {
		cmd = append([]string{bins.coreslot, "run", "--restart=yes", "--mem", "128M", "--"}, leaf...)
	}
	a := []string{bins.coreslot, "run", "--restart=yes", "--label", c.name}
	if c.timeout > 0 {
		a = append(a, "--timeout", c.timeout.String())
	}
	a = append(a, "--mem", fmt.Sprintf("%dM", c.need))
	return append(append(a, "--"), cmd...)
}

// after waits d while c's process lives: false when the storm ended or the process exited.
func (st *storm) after(ctx context.Context, c *stormClient, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-c.proc.done:
		return false
	case <-time.After(d):
		return true
	}
}

func (st *storm) release(c *stormClient) {
	_ = os.WriteFile(filepath.Join(st.e.MarkDir, c.name+".release"), nil, 0o600)
}

func (st *storm) holding(c *stormClient) bool {
	_, ok := readReport(st.e.MarkDir, c.name)
	return ok
}

// firstWaveHolds reports whether every first-wave client holds.
func (st *storm) firstWaveHolds() bool {
	for id := 1; id <= firstWave; id++ {
		if !st.holding(st.plan(id)) {
			return false
		}
	}
	return true
}

// waitHolding waits until c's command runs: false when the storm ended or the process exited.
func (st *storm) waitHolding(ctx context.Context, c *stormClient) bool {
	for !st.holding(c) {
		if !st.after(ctx, c, pollStep) {
			return false
		}
	}
	return true
}

func (st *storm) kill(c *stormClient) {
	held := st.holding(c)
	if c.proc.kill(-c.proc.pid, syscall.SIGKILL) {
		c.killed = true
		st.count("SIGKILL")
		if held {
			st.count("SIGKILL of a holder")
		}
	}
}

// life runs one client through its fate. However it ends, the client is continued, released
// and killed with its process group, and reaped, before life returns.
func (st *storm) life(ctx context.Context, c *stormClient) {
	c.proc = st.e.StartCommand(st.argv(c)...)
	c.proc.name = c.name
	st.mu.Lock()
	st.live[c] = true
	st.mu.Unlock()
	st.count("client " + c.kind)
	defer st.finish(c)

	switch c.fate {
	case "kill":
		if st.after(ctx, c, c.at) {
			st.kill(c)
		}
		return
	case "killheld":
		if st.waitHolding(ctx, c) && st.after(ctx, c, c.at) {
			st.kill(c)
		}
		return
	case "stop":
		if !st.after(ctx, c, c.at) || !c.proc.kill(c.proc.pid, syscall.SIGSTOP) {
			return
		}
		c.stopped.Store(true)
		st.count("SIGSTOP")
		if c.stopFor > 10*time.Second {
			st.count("SIGSTOP past the STALLED bound")
		}
		ok := st.after(ctx, c, c.stopFor)
		c.proc.kill(c.proc.pid, syscall.SIGCONT)
		c.stopped.Store(false)
		if !ok {
			return
		}
	case "cancel":
		if !st.after(ctx, c, c.at) {
			return
		}
		st.cancel(ctx, c)
	}
	// Hold for a while once the command runs, then let it go.
	if !st.waitHolding(ctx, c) {
		return
	}
	if c.afterShrink {
		select {
		case <-ctx.Done():
			return
		case <-c.proc.done:
			return
		case <-st.shrunk:
		}
	}
	if !st.after(ctx, c, c.hold) {
		return
	}
	if c.fate == "killheld-after-shrink" {
		st.mu.Lock()
		st.victim = c.kind
		st.mu.Unlock()
		st.kill(c)
		return
	}
	st.release(c)
	select {
	case <-ctx.Done():
	case <-c.proc.done:
	}
}

// ticketID returns the id of c's ticket while it is queued.
func (st *storm) ticketID(c *stormClient) (string, bool) {
	entries, _ := os.ReadDir(filepath.Join(st.e.Dir, store.QueueDir))
	for _, en := range entries {
		if n := en.Name(); strings.HasSuffix(n, "-"+c.name) && len(n) > 38 {
			return n[:38], true
		}
	}
	return "", false
}

// cancel waits up to 2 s for c's ticket, then publishes a cancel request for it through the CLI
// by label. A ticket admitted meanwhile, or gone, makes it a no-op. The cancel command is killed if it has not exited within exitWait, and is
// reaped before the storm ends.
func (st *storm) cancel(ctx context.Context, c *stormClient) {
	_, queued := st.ticketID(c)
	for end := time.Now().Add(2 * time.Second); !queued && time.Now().Before(end); _, queued = st.ticketID(c) {
		if st.holding(c) || !st.after(ctx, c, pollStep) {
			return
		}
	}
	if !queued {
		return
	}
	p := st.e.Start("cancel", "--label", c.name)
	st.mu.Lock()
	st.helpers = append(st.helpers, p)
	st.mu.Unlock()
	st.count("cancel")
	select {
	case <-p.done:
		if p.code == 0 {
			st.count("cancel published")
		}
	case <-time.After(exitWait):
		p.kill(-p.pid, syscall.SIGKILL)
		st.t.Errorf("cancel of %s did not exit within %v; killed", c.name, exitWait)
	}
}

// cancelledMessage is what a client prints on stderr when a cancel request ended its wait: the
// evidence, besides the refusal code, that the cancel acted.
const cancelledMessage = "coreslot: the request was cancelled"

// canceller cancels one client while it is seen queued: it picks a live CLI
// client with a ticket, no timeout and a command that does not run, publishes a cancel request
// for it by label, and checks that the client then exits with the refusal code and the
// canceled message. A request that loses the race (the client was admitted first) is retried
// with another queued client, at most cancelTries times and only while the storm runs; none
// found or none taking effect leaves the floor to fail by name.
func (st *storm) canceller(ctx context.Context) {
	const cancelTries = 10
	select {
	case <-ctx.Done():
		return
	case <-st.shrunk: // the queue fills once the first shrink holds admissions back
	}
	r := rand.New(rand.NewPCG(st.seed, victimStream-1))
	for tries := 0; tries < cancelTries && ctx.Err() == nil; {
		var queued []*stormClient
		st.mu.Lock()
		for c := range st.live {
			// A client with a timeout could exit with the refusal code on its own.
			if c.timeout > 0 || c.stopped.Load() || c.cancelled.Load() || c.proc.Exited() ||
				st.holding(c) {
				continue
			}
			if _, ok := st.ticketID(c); ok {
				queued = append(queued, c)
			}
		}
		st.mu.Unlock()
		if len(queued) == 0 {
			st.count("canceller found no queued client")
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		slices.SortFunc(queued, func(a, b *stormClient) int { return a.id - b.id })
		c := queued[r.IntN(len(queued))]
		tries++
		c.cancelled.Store(true)
		p := st.e.Start("cancel", "--label", c.name)
		st.mu.Lock()
		st.helpers = append(st.helpers, p)
		st.mu.Unlock()
		select {
		case <-p.done:
		case <-time.After(exitWait):
			p.kill(-p.pid, syscall.SIGKILL)
			st.t.Errorf("cancel of %s did not exit within %v; killed", c.name, exitWait)
			continue
		}
		if p.code != 0 {
			c.cancelled.Store(false) // admitted or gone before the request, nothing published
			continue
		}
		st.count("cancel published")
		select {
		case <-c.proc.done:
			if c.proc.code == exitRefusal && strings.Contains(c.proc.Stderr(), cancelledMessage) {
				st.count("cancel took effect")
				st.count("cancel took effect on " + c.kind)
				return
			}
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// finish continues, releases, kills and reaps c, and records how it ended.
func (st *storm) finish(c *stormClient) {
	st.mu.Lock()
	delete(st.live, c)
	st.mu.Unlock()
	if c.stopped.Load() {
		c.proc.kill(c.proc.pid, syscall.SIGCONT)
	}
	st.release(c) // a nested chain orphaned by a kill lets go of its grant too
	if c.proc.kill(-c.proc.pid, syscall.SIGKILL) {
		c.killed = true
	}
	select {
	case <-c.proc.done:
	case <-time.After(windDown):
		st.t.Errorf("%s (pid %d) was not reaped within %v after SIGKILL", c.name, c.proc.pid, windDown)
		return
	}
	res := stormResult{c: c, code: c.proc.code, stderr: c.proc.Stderr(), admitted: st.holding(c)}
	c.proc.stdout.Close()
	c.proc.stderr.Close()
	if res.admitted {
		st.count("admitted cli")
		if c.inner != "" {
			st.count("admitted nested")
		}
	}
	st.mu.Lock()
	st.results = append(st.results, res)
	st.mu.Unlock()
}

// setRoom changes the room to mib and counts the change: shrinks, growths, and shrinks that
// leave more reserved than the new room.
func (st *storm) setRoom(ctx context.Context, mib int) bool {
	old, next := st.s.Room(), int64(mib)
	reserved, err := st.s.SetRoom(ctx, next)
	if err != nil {
		return false
	}
	switch {
	case next < old:
		st.count("shrink")
		if reserved > next {
			st.count("shrink below what is reserved")
		}
	case next > old:
		st.count("growth")
	}
	return true
}

// reconfigure changes the configuration: first a shrink of the room from 1792 to 896 MiB once the
// first wave holds (or 5 s passed) and a sample saw it hold, then a growth back to 1792, then
// random rooms of stormRoomMin..stormRoomMax every 1.5 to 4 s.
func (st *storm) reconfigure(ctx context.Context) {
	shrunk := false
	defer func() {
		if !shrunk {
			close(st.shrunk)
		}
	}()
	sleep := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		all := true
		for id := 1; id <= 4; id++ {
			if _, ok := readReport(st.e.MarkDir, st.plan(id).name); !ok {
				all = false
			}
		}
		if all || !sleep(pollStep) {
			break
		}
	}
	_, _ = st.s.Sample(ctx) // the first wave's 1792 MiB, before the shrink
	if ctx.Err() != nil || !st.setRoom(ctx, stormRoomMin/2) {
		return
	}
	shrunk = true
	close(st.shrunk)
	r := rand.New(rand.NewPCG(st.seed, 0))
	if !sleep(time.Second+time.Duration(r.IntN(1500))*time.Millisecond) ||
		!st.setRoom(ctx, stormRoomMin) {
		return
	}
	for sleep(1500*time.Millisecond + time.Duration(r.IntN(2500))*time.Millisecond) {
		if !st.setRoom(ctx, stormRoomMin+r.IntN(stormRoomMax-stormRoomMin+1)) {
			return
		}
	}
}

// stragglers lists the processes whose command line names dir, the storm's marker directory,
// which every client's command line contains: from /proc where there is one, else from pgrep.
func stragglers(t *testing.T, dir string) []int {
	t.Helper()
	var pids []int
	if entries, err := os.ReadDir("/proc"); err == nil {
		for _, en := range entries {
			pid, err := strconv.Atoi(en.Name())
			if err != nil || pid == os.Getpid() {
				continue
			}
			cmdline, err := os.ReadFile(filepath.Join("/proc", en.Name(), "cmdline"))
			if err == nil && strings.Contains(string(cmdline), dir) {
				pids = append(pids, pid)
			}
		}
		return pids
	}
	out, err := exec.Command("pgrep", "-f", regexp.QuoteMeta(dir)).Output()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) { // 1: no process matched
		t.Fatalf("pgrep: %v", err)
	}
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids
}

// stderrAnomalies are diagnostics no storm client may print: the storm's state directory is
// always usable, every nested run is under a valid grant, and nothing crashes.
var stderrAnomalies = []string{"state error", "fail-open", "invalid nested context", "Traceback",
	"panic:", "degraded"}

func TestKillStorm(t *testing.T) {
	dur, clients := stormFull, clientsFull
	if testing.Short() {
		dur, clients = stormShort, clientsShort
	}
	seed := uint64(time.Now().UnixNano())
	if v := os.Getenv(stormSeedEnv); v != "" {
		var err error
		if seed, err = strconv.ParseUint(v, 10, 64); err != nil {
			t.Fatalf("%s=%q: %v", stormSeedEnv, v, err)
		}
	}
	e := State(t)
	st := &storm{t: t, e: e, seed: seed, short: testing.Short(),
		shrunk: make(chan struct{}),
		live:   map[*stormClient]bool{}, counts: map[string]int{}}
	banner := fmt.Sprintf("kill-storm seed %d (%s=%d replays the schedule; -short=%v), %d clients for %v; load: %s",
		seed, stormSeedEnv, seed, testing.Short(), clients, dur, uptime())
	t.Log(banner)
	fmt.Fprintln(os.Stderr, banner)
	defer func() {
		if t.Failed() {
			t.Logf("kill-storm seed %d failed; replay with %s=%d", seed, stormSeedEnv, seed)
		}
	}()

	st.s = NewSampler(t, e, stormRoomMin)

	sctx, stopSampler := context.WithCancel(context.Background())
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		st.s.Run(sctx)
	}()
	defer func() { stopSampler(); <-sampled }()

	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	st.started = time.Now()
	var wg sync.WaitGroup
	wg.Go(func() { st.reconfigure(ctx) })
	// Every client after the first wave starts once the first wave holds (or after 5 s). Two
	// floors need that: the first shrink must find the first wave's reservations to land below
	// what is reserved, and the victim must be admitted, which a later request that
	// blocks the queue could otherwise delay past a -short run.
	waveHolds := make(chan struct{})
	go func() {
		defer close(waveHolds)
		for end := time.Now().Add(5 * time.Second); ctx.Err() == nil && time.Now().Before(end); time.Sleep(pollStep) {
			if st.firstWaveHolds() {
				return
			}
		}
	}()
	wg.Go(func() { st.canceller(ctx) })
	for slot := range clients {
		wg.Go(func() {
			delay := time.Duration(slot) * 40 * time.Millisecond // stagger the first arrivals
			if slot >= firstWave {
				select {
				case <-ctx.Done():
					return
				case <-waveHolds:
				}
				delay = time.Duration(slot-firstWave) * 40 * time.Millisecond
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			for ctx.Err() == nil {
				st.mu.Lock()
				st.nextID++
				id := st.nextID
				st.mu.Unlock()
				st.life(ctx, st.plan(id))
			}
		})
	}
	wg.Wait()

	// Every client is reaped. Two more samples confirm whatever the last ones found, then the
	// sampler stops.
	for range 2 {
		sc, done := context.WithTimeout(context.Background(), startWait)
		_, _ = st.s.Sample(sc)
		done()
	}
	stopSampler()
	<-sampled
	for _, p := range st.helpers {
		select {
		case <-p.done:
		case <-time.After(exitWait):
			p.kill(-p.pid, syscall.SIGKILL)
			t.Errorf("cancel command pid %d still runs after the storm; killed", p.pid)
		}
	}

	// Process hygiene: nothing the storm started, directly or through a run, survives it.
	deadline := time.Now().Add(5 * time.Second) // a reparented command exits within 20 ms
	for left := stragglers(t, e.MarkDir); len(left) > 0; left = stragglers(t, e.MarkDir) {
		if time.Now().After(deadline) {
			for _, pid := range left {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			t.Errorf("processes outlived the storm: %v", left)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	lctx, ldone := context.WithTimeout(context.Background(), startWait)
	defer ldone()
	holds, err := st.s.Leftovers(lctx)
	if err != nil {
		t.Fatalf("final observation: %v", err)
	}
	if len(holds) > 0 {
		t.Errorf("after every client is gone: live holds %v", holds)
	}
	if rep := e.Status(); rep.Degraded != nil {
		t.Errorf("degraded mode after the storm: %+v", *rep.Degraded)
	}

	codes := map[string]int{}
	for _, r := range st.results {
		key := fmt.Sprintf("%s exit %d", r.c.kind, r.code)
		if r.c.killed {
			key = r.c.kind + " killed"
		}
		codes[key]++
		refusalExpected := r.c.fate == "cancel" || r.c.timeout > 0 || r.c.cancelled.Load()
		if !r.c.killed && r.code != 0 && (r.code != exitRefusal || !refusalExpected) {
			t.Errorf("%s (pid %d, fate %s) exited %d; stderr:\n%s", r.c.name, r.c.proc.pid, r.c.fate, r.code, r.stderr)
			continue
		}
		for _, bad := range stderrAnomalies {
			if strings.Contains(r.stderr, bad) {
				t.Errorf("%s (pid %d, fate %s, exit %d) printed %q:\n%s", r.c.name, r.c.proc.pid, r.c.fate, r.code, bad, r.stderr)
				break
			}
		}
	}
	violations, samples, missed := st.s.Violations()
	for _, v := range violations {
		t.Errorf("seed %d: %v", seed, v)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	summary := fmt.Sprintf("kill-storm seed %d: %d clients in %v, %d samples (%d missed), %d violations; "+
		"holder killed: %s; events %v; endings %v; load after: %s",
		seed, len(st.results), time.Since(st.started).Round(time.Second), samples, missed, len(violations),
		st.victim, st.counts, codes, uptime())
	t.Log(summary)
	fmt.Fprintln(os.Stderr, summary)

	// The run exercised what it is for: floors on what happened, scaled for -short. The first
	// shrink is deterministic (1792 MiB to 896 while the first wave holds 1792), so the floor on a
	// shrink below what is reserved checks that the schedule ran, not that chance produced one.
	admitted := 0
	for k, n := range st.counts {
		if strings.HasPrefix(k, "admitted ") && k != "admitted nested" {
			admitted += n
		}
	}
	minAdmitted := 25
	if testing.Short() {
		minAdmitted = 6
	}
	if admitted < minAdmitted {
		t.Errorf("%d admissions, want at least %d", admitted, minAdmitted)
	}
	floors := []string{"admitted cli", "admitted nested", "SIGKILL of a holder",
		"SIGSTOP", "cancel took effect", "shrink below what is reserved", "growth"}
	floorFailed := false
	for _, what := range floors {
		if st.counts[what] < 1 {
			floorFailed = true
			t.Errorf("the storm never exercised %q", what)
			if what == "cancel took effect" {
				t.Logf("canceller: %d cancel requests published by any client, %d canceller looks found no queued client",
					st.counts["cancel published"], st.counts["canceller found no queued client"])
			}
		}
	}
	if floorFailed {
		for _, r := range st.results {
			if r.c.id <= firstWave {
				t.Logf("first wave %s (pid %d, fate %s): exit %d, killed %v, admitted %v; stderr:\n%s",
					r.c.name, r.c.proc.pid, r.c.fate, r.code, r.c.killed, r.admitted, r.stderr)
			}
		}
	}
	if minSamples := int(3 * dur.Seconds()); samples < minSamples || missed > 3 {
		t.Errorf("%d samples (%d missed), want at least %d and at most 3 missed", samples, missed, minSamples)
	}
}
