//go:build coreslot_testhooks && unix

package conformance

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/store"
)

// The kill-storm: randomized CLI, library and independent Python clients,
// some of them nested runs and some token-only, queue for two resources while the storm kills
// them with SIGKILL, stops and continues them, cancels them, and shrinks and grows the sizes
// through the configuration. A sampler checks I1, I2, I3 and I7 throughout. Every run prints its
// seed; CORESLOT_STORM_SEED=<seed> replays the same schedule (each client's kind, request, fate
// and delays, and the size changes; the interleaving is the scheduler's). The schedule also
// depends on -short.

const (
	stormFull, stormShort     = 60 * time.Second, 10 * time.Second
	clientsFull, clientsShort = 50, 20
	stormSeedEnv              = "CORESLOT_STORM_SEED"
	// tokenOnlyReportBound is how long a locked token listed by no hold may be put down to a
	// token-only run whose command has not reported its descriptors yet. Past it the token
	// counts against I2 like any other.
	tokenOnlyReportBound = 10 * time.Second
	// windDown bounds the end of the storm: every client killed and reaped.
	windDown = 60 * time.Second
)

// A storm label names its request, so that the sampler can check I7 from the hold alone:
// s<id>.<kind>.c<min>_<max>[.i<min>_<max>], cpu and io. It uses only characters a hold's display
// keeps, so the display of a ticket's name is the label itself.
var labelNeed = regexp.MustCompile(`\.([ci])([0-9]+)_([0-9]+)`)

var labelResource = map[string]string{"c": "cpu", "i": "io"}

func stormLabel(id int, kind string, need map[string]spec.Range) string {
	l := fmt.Sprintf("s%05d.%s", id, kind)
	for _, short := range []string{"c", "i"} {
		if rg, ok := need[labelResource[short]]; ok {
			l += fmt.Sprintf(".%s%d_%d", short, rg.Min, rg.Max)
		}
	}
	return l
}

// stormRequest is the request a storm label names.
func stormRequest(label string) (map[string]spec.Range, bool) {
	ms := labelNeed.FindAllStringSubmatch(label, -1)
	if len(ms) == 0 {
		return nil, false
	}
	need := map[string]spec.Range{}
	for _, m := range ms {
		lo, _ := strconv.Atoi(m[2])
		hi, _ := strconv.Atoi(m[3])
		need[labelResource[m[1]]] = spec.Range{Min: lo, Max: hi}
	}
	return need, true
}

// ---------------------------------------------------------------- the library client binary

var libBuild struct {
	once sync.Once
	path string
	err  error
}

// buildLibClient builds conformance/libclient beside the harness's binaries.
func buildLibClient(t *testing.T) string {
	t.Helper()
	Build(t)
	libBuild.once.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			libBuild.err = err
			return
		}
		out := filepath.Join(buildDir, "libclient")
		cmd := exec.Command("go", "build", "-tags", "coreslot_testhooks", "-o", out, "./conformance/libclient")
		cmd.Dir = root
		if msg, err := cmd.CombinedOutput(); err != nil {
			libBuild.err = fmt.Errorf("go build ./conformance/libclient: %v\n%s", err, msg)
			return
		}
		libBuild.path = out
	})
	if libBuild.err != nil {
		t.Fatal(libBuild.err)
	}
	return libBuild.path
}

// pyClient returns the interpreter and the independent client's path, or why there are none.
func pyClient(t *testing.T) (py, client string, err error) {
	t.Helper()
	if py, err = python(); err != nil {
		if os.Getenv("CORESLOT_REQUIRE_PYTHON") == "1" {
			t.Fatalf("CORESLOT_REQUIRE_PYTHON=1 and no Python 3.11+: %v", err)
		}
		return "", "", err
	}
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	return py, filepath.Join(root, "conformance", "pyclient", "coreslot_client.py"), nil
}

// ---------------------------------------------------------------- the storm

type stormClient struct {
	id       int
	kind     string // cli, py, lib, tok; nested runs are "<outer>-<inner>", like cli-py
	outer    string // cli, py, lib or tok
	inner    string // "", go or py: the nested run's client
	name     string // the label, and the marker name of its command
	need     map[string]spec.Range
	priority int
	timeout  time.Duration
	fate     string // release, kill, stop or cancel
	at       time.Duration
	stopFor  time.Duration
	hold     time.Duration
	cancelPy bool

	proc    *Proc
	killed  bool // the storm sent it SIGKILL
	stopped bool
}

type stormResult struct {
	c      *stormClient
	code   int
	stderr string
}

type storm struct {
	t       *testing.T
	e       Env
	s       *Sampler
	seed    uint64
	short   bool
	py      string // "" without Python 3.11+
	pyc     string
	libBin  string
	started time.Time

	mu      sync.Mutex
	nextID  int
	tokens  []*stormClient // every token-only client, for the sampler's exemption
	labels  map[string]map[string]spec.Range
	results []stormResult
	counts  map[string]int
}

func (st *storm) count(what string) {
	st.mu.Lock()
	st.counts[what]++
	st.mu.Unlock()
}

// plan draws client id's schedule from its own stream of the seed, so that a client's plan does
// not depend on the order in which the slots ask for theirs.
func (st *storm) plan(id int) *stormClient {
	r := rand.New(rand.NewPCG(st.seed, uint64(id)))
	c := &stormClient{id: id, need: map[string]spec.Range{}}
	outers := []string{"cli", "cli", "cli", "lib", "lib", "lib", "tok"}
	inners := []string{"go"}
	if st.py != "" {
		outers = append(outers, "py", "py", "py")
		inners = append(inners, "py")
	}
	c.outer = outers[r.IntN(len(outers))]
	if c.outer != "tok" && r.IntN(6) == 0 {
		c.inner = inners[r.IntN(len(inners))]
	}
	c.kind = c.outer
	if c.inner != "" {
		c.kind += "-" + c.inner
	}
	switch {
	case c.outer == "tok":
		c.need["cpu"] = spec.Range{Min: 1, Max: 1 + r.IntN(2)}
	case r.IntN(12) == 0:
		c.need["cpu"] = spec.Range{Min: 9, Max: 9} // above every size the storm sets: clamped
	default:
		lo := 1 + r.IntN(3)
		c.need["cpu"] = spec.Range{Min: lo, Max: lo + r.IntN(3)}
	}
	if c.outer != "tok" && r.IntN(10) < 4 {
		lo := 1 + r.IntN(2)
		c.need["io"] = spec.Range{Min: lo, Max: lo + r.IntN(2)}
	}
	if c.outer != "tok" && r.IntN(5) == 0 {
		c.priority = r.IntN(5) - 2
	}
	if c.outer != "tok" && r.IntN(5) == 0 {
		c.timeout = time.Second + time.Duration(r.IntN(5000))*time.Millisecond
	}
	switch x := r.IntN(100); {
	case x < 50:
		c.fate = "release"
	case x < 65:
		c.fate = "kill"
	case x < 82:
		c.fate = "stop"
	default:
		c.fate = "cancel"
	}
	if c.outer == "tok" && (c.fate == "stop" || c.fate == "cancel") {
		c.fate = "release" // a token-only run has no ticket, and stopping it proves nothing more
	}
	c.at = time.Duration(r.IntN(4000)) * time.Millisecond
	c.stopFor = 100*time.Millisecond + time.Duration(r.IntN(2900))*time.Millisecond
	if !st.short && r.IntN(6) == 0 {
		c.stopFor = 10500*time.Millisecond + time.Duration(r.IntN(2500))*time.Millisecond // STALLED
	}
	c.hold = 50*time.Millisecond + time.Duration(r.IntN(1950))*time.Millisecond
	c.cancelPy = st.py != "" && r.IntN(2) == 0
	c.name = stormLabel(id, c.kind, c.need)
	return c
}

func needFlags(need map[string]spec.Range, flag string) []string {
	var out []string
	for _, r := range slices.Sorted(maps.Keys(need)) {
		out = append(out, flag, fmt.Sprintf("%s=%d..%d", r, need[r].Min, need[r].Max))
	}
	return out
}

// argv is c's command line.
func (st *storm) argv(c *stormClient) []string {
	leaf := []string{bins.child, "hold", st.e.MarkDir, c.name}
	cmd := leaf
	switch c.inner {
	case "go":
		cmd = append([]string{bins.coreslot, "run", "--need", "cpu=1..1", "--"}, leaf...)
	case "py":
		cmd = append([]string{st.py, st.pyc, "run", "--need", "cpu=1..1", "--"}, leaf...)
	}
	switch c.outer {
	case "cli", "tok":
		a := []string{bins.coreslot, "run", "--label", c.name}
		if c.outer == "tok" {
			// A client implementing only format 2 finds format 1 and runs token-only.
			a = append([]string{"/usr/bin/env", "CORESLOT_TESTHOOK_FORMATS=2"}, a...)
		}
		if c.priority != 0 {
			a = append(a, "--priority", strconv.Itoa(c.priority))
		}
		if c.timeout > 0 {
			a = append(a, "--timeout", c.timeout.String())
		}
		a = append(a, needFlags(c.need, "--need")...)
		return append(append(a, "--"), cmd...)
	case "py":
		a := []string{st.py, st.pyc, "run", "--label", c.name}
		if c.priority != 0 {
			a = append(a, "--priority", strconv.Itoa(c.priority))
		}
		if c.timeout > 0 {
			a = append(a, "--timeout", strconv.FormatFloat(c.timeout.Seconds(), 'f', 3, 64))
		}
		a = append(a, needFlags(c.need, "--need")...)
		return append(append(a, "--"), cmd...)
	default: // lib
		a := []string{st.libBin, "-dir", st.e.Dir, "-mark", st.e.MarkDir, "-name", c.name, "-label", c.name,
			"-priority", strconv.Itoa(c.priority), "-timeout", c.timeout.String()}
		a = append(a, needFlags(c.need, "-need")...)
		if c.inner != "" {
			a = append(append(a, "--"), cmd...)
		}
		return a
	}
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

// life runs one client through its fate. However it ends, the client is continued, released
// and killed with its process group, and reaped, before life returns.
func (st *storm) life(ctx context.Context, c *stormClient) {
	st.mu.Lock()
	st.labels[c.name] = maps.Clone(c.need)
	st.mu.Unlock()
	c.proc = st.e.StartCommand(st.argv(c)...)
	c.proc.name = c.name
	if c.outer == "tok" {
		st.mu.Lock()
		st.tokens = append(st.tokens, c)
		st.mu.Unlock()
	}
	st.count("client " + c.kind)
	defer st.finish(c)

	switch c.fate {
	case "kill":
		if st.after(ctx, c, c.at) && c.proc.kill(-c.proc.pid, syscall.SIGKILL) {
			c.killed = true
			st.count("SIGKILL")
		}
		return
	case "stop":
		if !st.after(ctx, c, c.at) || !c.proc.kill(c.proc.pid, syscall.SIGSTOP) {
			return
		}
		c.stopped = true
		st.count("SIGSTOP")
		if c.stopFor > 10*time.Second {
			st.count("SIGSTOP past the STALLED bound")
		}
		ok := st.after(ctx, c, c.stopFor)
		c.proc.kill(c.proc.pid, syscall.SIGCONT)
		c.stopped = false
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
	for {
		if _, ok := st.e.Started(c.name); ok {
			break
		}
		if !st.after(ctx, c, pollStep) {
			return
		}
	}
	st.count("admitted")
	if !st.after(ctx, c, c.hold) {
		return
	}
	st.release(c)
	select {
	case <-ctx.Done():
	case <-c.proc.done:
	}
}

// cancel publishes a cancel request for c's ticket, through the Go CLI by label or through the
// Python client by id. A ticket already admitted or gone makes it a no-op.
func (st *storm) cancel(ctx context.Context, c *stormClient) {
	var p *Proc
	if c.cancelPy {
		id := ""
		entries, _ := os.ReadDir(filepath.Join(st.e.Dir, store.QueueDir))
		for _, en := range entries {
			if strings.HasSuffix(en.Name(), "-"+c.name) && len(en.Name()) > 38 {
				id = en.Name()[:38]
			}
		}
		if id == "" {
			return
		}
		p = st.e.StartCommand(st.py, st.pyc, "cancel", id)
	} else {
		p = st.e.Start("cancel", "--label", c.name)
	}
	st.count("cancel")
	select {
	case <-p.done:
		if p.code == 0 {
			st.count("cancel published")
		}
	case <-ctx.Done():
	case <-time.After(exitWait):
	}
}

// finish continues, releases, kills and reaps c, and records how it ended.
func (st *storm) finish(c *stormClient) {
	if c.stopped {
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
	res := stormResult{c: c, code: c.proc.code, stderr: c.proc.Stderr()}
	c.proc.stdout.Close()
	c.proc.stderr.Close()
	st.mu.Lock()
	st.results = append(st.results, res)
	st.mu.Unlock()
}

// exempt is the sampler's exemption for token-only grants: a locked token whose inode a live
// token-only command reported among its descriptors, or, for at most tokenOnlyReportBound, any
// unlisted token while a token-only run that may have been admitted has not reported yet.
func (st *storm) exempt(k Token, inode [2]uint64, unlisted time.Duration) bool {
	st.mu.Lock()
	toks := slices.Clone(st.tokens)
	st.mu.Unlock()
	for _, c := range toks {
		rep, ok := st.e.Started(c.name)
		if !ok {
			if !c.proc.Exited() && unlisted < tokenOnlyReportBound {
				return true
			}
			continue
		}
		if c.proc.Exited() && !pidAlive(rep.PID) {
			continue
		}
		for fd, id := range rep.FDs {
			if fd >= 3 && id == inode {
				return true
			}
		}
	}
	return false
}

func pidAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

func (st *storm) request(label string) (map[string]spec.Range, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	need, ok := st.labels[label]
	return need, ok
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
	st := &storm{t: t, e: e, seed: seed, short: testing.Short(), libBin: buildLibClient(t),
		labels: map[string]map[string]spec.Range{}, counts: map[string]int{}}
	py, pyc, err := pyClient(t)
	if err != nil {
		t.Logf("the independent Python client is not exercised: no Python 3.11+ (%v)", err)
	} else {
		st.py, st.pyc = py, pyc
	}
	banner := fmt.Sprintf("kill-storm seed %d (%s=%d replays the schedule; -short=%v), %d clients for %v, python %v; load: %s",
		seed, stormSeedEnv, seed, testing.Short(), clients, dur, st.py != "", uptime())
	t.Log(banner)
	fmt.Fprintln(os.Stderr, banner)
	defer func() {
		if t.Failed() {
			t.Logf("kill-storm seed %d failed; replay with %s=%d", seed, stormSeedEnv, seed)
		}
	}()

	sizes := map[string]int{"cpu": 4, "io": 2}
	st.s = NewSampler(t, e, sizes)
	st.s.Request = st.request
	st.s.Exempt = st.exempt

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
	wg.Go(func() { // the configuration: shrinks and growths
		r := rand.New(rand.NewPCG(seed, 0))
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(1500*time.Millisecond + time.Duration(r.IntN(2500))*time.Millisecond):
			}
			old := st.s.Sizes()
			next := map[string]int{"cpu": 2 + r.IntN(7), "io": 1 + r.IntN(3)}
			if err := st.s.Resize(ctx, next); err != nil {
				return
			}
			for res, n := range next {
				switch {
				case n < old[res]:
					st.count("shrink")
				case n > old[res]:
					st.count("growth")
				}
			}
		}
	})
	for slot := range clients {
		wg.Go(func() {
			select { // stagger the first arrivals
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(slot) * 40 * time.Millisecond):
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
	locked, holds, err := st.s.Leftovers(lctx)
	if err != nil {
		t.Fatalf("final observation: %v", err)
	}
	if len(locked) > 0 || len(holds) > 0 {
		t.Errorf("after every client is gone: locked tokens %v, live holds %v", locked, holds)
	}
	if rep := e.Status(); rep.Degraded != nil {
		t.Errorf("degraded mode after the storm: %+v", *rep.Degraded)
	}

	codes := map[string]int{}
	for _, r := range st.results {
		key := fmt.Sprintf("%s exit %d", r.c.outer, r.code)
		if r.c.killed {
			key = r.c.outer + " killed"
		}
		codes[key]++
		if !r.c.killed && r.code != 0 && r.code != exitRefusal {
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
	summary := fmt.Sprintf("kill-storm seed %d: %d clients in %v, %d samples (%d missed), %d violations; events %v; endings %v; load after: %s",
		seed, len(st.results), time.Since(st.started).Round(time.Second), samples, missed, len(violations),
		st.counts, codes, uptime())
	t.Log(summary)
	fmt.Fprintln(os.Stderr, summary)
	if samples < 10 {
		t.Errorf("only %d samples were taken", samples)
	}
}
