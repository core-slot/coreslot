//go:build coreslot_testhooks && unix

package conformance

import (
	"context"
	"encoding/json"
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

// The kill-storm of spec section 10.2: randomized CLI, library and independent Python clients,
// some of them nested runs and some token-only, queue for two resources while the storm kills
// them with SIGKILL, stops and continues them, cancels them, and shrinks and grows the sizes
// through the configuration. A sampler checks I1, I2, I3 and I7 throughout. Every run prints its
// seed; CORESLOT_STORM_SEED=<seed> replays the same schedule (each client's kind, request, fate
// and delays, and the size changes; the interleaving is the scheduler's). The schedule also
// depends on -short. The first clients and the first two size changes are fixed, so that every
// run exercises each kind of client, a kill of a holder, a stop, a cancellation, a shrink below
// the locked count and a growth; the test asserts that it did.

const (
	stormFull, stormShort     = 60 * time.Second, 10 * time.Second
	clientsFull, clientsShort = 50, 20
	stormSeedEnv              = "CORESLOT_STORM_SEED"
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

// ---------------------------------------------------------------- token-only runs

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

// tokenOnlyRun is a token-only run the test started: it publishes nothing. Its command
// creates NAME.begun in the marker directory as its very first action, so the run is known to
// be admitted, and then reports the descriptors it inherited in NAME.started.
type tokenOnlyRun struct {
	name  string
	need  map[string]spec.Range
	alive func(rep ChildReport, reported bool) bool // the run, or its reported command, lives
}

// tokenOnlyRuns are the token-only runs of one state directory, for the sampler's TokenOnly.
type tokenOnlyRuns struct {
	markDir string

	mu   sync.Mutex
	runs []*tokenOnlyRun
}

func (r *tokenOnlyRuns) add(name string, need map[string]spec.Range, alive func(ChildReport, bool) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, &tokenOnlyRun{name: name, need: need, alive: alive})
}

// begunPath is the start marker of the token-only command named name.
func begunPath(markDir, name string) string { return filepath.Join(markDir, name+".begun") }

// grants is the sampler's TokenOnly: the inodes the live runs' commands reported, and the
// minimums of the live runs that were admitted (their command began) and have not reported.
// A run still waiting has neither.
func (r *tokenOnlyRuns) grants() TokenOnly {
	r.mu.Lock()
	runs := slices.Clone(r.runs)
	r.mu.Unlock()
	out := TokenOnly{Inodes: map[[2]uint64]bool{}}
	for _, run := range runs {
		rep, reported := readReport(r.markDir, run.name)
		if !run.alive(rep, reported) {
			continue
		}
		if reported {
			for fd, id := range rep.FDs {
				if fd >= 3 {
					out.Inodes[id] = true
				}
			}
			continue
		}
		if _, err := os.Stat(begunPath(r.markDir, run.name)); err != nil {
			continue // waiting: no grant
		}
		mins := map[string]int{}
		for res, rg := range run.need {
			mins[res] = rg.Min
		}
		out.Admitted = append(out.Admitted, mins)
	}
	return out
}

// reportLatency is the longest time, over the runs that reported, from a command's start
// marker to its report.
func (r *tokenOnlyRuns) reportLatency() (longest time.Duration, runs int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, run := range r.runs {
		b, err1 := os.Stat(begunPath(r.markDir, run.name))
		s, err2 := os.Stat(filepath.Join(r.markDir, run.name+".started"))
		if err1 != nil || err2 != nil {
			continue
		}
		runs++
		longest = max(longest, s.ModTime().Sub(b.ModTime()))
	}
	return longest, runs
}

// ---------------------------------------------------------------- the storm

type stormClient struct {
	id          int
	kind        string // cli, py, lib, tok; nested runs are "<outer>-<inner>", like cli-py
	outer       string // cli, py, lib or tok
	inner       string // "", go or py: the nested run's client
	name        string // the label, and the marker name of its command
	need        map[string]spec.Range
	priority    int
	timeout     time.Duration
	fate        string // release, kill (at a time), killheld (once it holds), stop or cancel
	at          time.Duration
	stopFor     time.Duration
	hold        time.Duration
	cancelPy    bool
	afterShrink bool // a first-wave client: holds until the first shrink

	proc    *Proc
	killed  bool // the storm sent it SIGKILL
	stopped bool
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
	py      string // "" without Python 3.11+
	pyc     string
	libBin  string
	started time.Time
	tok     *tokenOnlyRuns
	shrunk  chan struct{} // closed after the first shrink, or when the storm ends without it

	mu      sync.Mutex
	nextID  int
	labels  map[string]map[string]spec.Range
	results []stormResult
	helpers []*Proc // cancel commands
	counts  map[string]int
}

func (st *storm) count(what string) {
	st.mu.Lock()
	st.counts[what]++
	st.mu.Unlock()
}

// firstWave is the number of fixed clients at the start of every run.
const firstWave = 7

// plan draws client id's schedule from its own stream of the seed, so that a client's plan does
// not depend on the order in which the slots ask for theirs. Clients 1 to firstWave are fixed:
// one admission of each kind (CLI, Python, a library run with a nested Go run, token-only), each
// holding one cpu token until the first shrink, then a CLI killed once it holds, a library
// waiter cancelled and a Python waiter stopped and continued.
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
	switch {
	case c.outer == "tok":
		c.need["cpu"] = spec.Range{Min: 1, Max: 1 + r.IntN(2)}
	case r.IntN(20) == 0:
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
	if c.outer == "tok" && (c.fate == "stop" || c.fate == "cancel") {
		c.fate = "release" // a token-only run has no ticket, and stopping it proves nothing more
	}
	c.at = time.Duration(r.IntN(4000)) * time.Millisecond
	c.stopFor = 100*time.Millisecond + time.Duration(r.IntN(2900))*time.Millisecond
	if !st.short && r.IntN(12) == 0 {
		c.stopFor = 10500*time.Millisecond + time.Duration(r.IntN(2500))*time.Millisecond // STALLED
	}
	c.hold = 50*time.Millisecond + time.Duration(r.IntN(1950))*time.Millisecond
	c.cancelPy = st.py != "" && r.IntN(2) == 0

	if id <= firstWave {
		py := "py"
		if st.py == "" {
			py = "cli"
		}
		c.outer = []string{"cli", py, "lib", "tok", "cli", "lib", py}[id-1]
		c.inner = ""
		c.need = map[string]spec.Range{"cpu": {Min: 1, Max: 1}}
		c.priority, c.timeout, c.fate = 0, 0, "release"
		switch id {
		case 1, 2, 3, 4:
			c.afterShrink = true
			if id == 3 {
				c.inner = "go"
			}
		case 5:
			c.fate, c.at = "killheld", 200*time.Millisecond
		case 6:
			c.fate, c.at, c.cancelPy = "cancel", 500*time.Millisecond, false
		case 7:
			c.fate, c.at, c.stopFor = "stop", 500*time.Millisecond, 1500*time.Millisecond
		}
	}
	c.kind = c.outer
	if c.inner != "" {
		c.kind += "-" + c.inner
	}
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
	if c.outer == "tok" {
		// The start marker first, then the command that reports its descriptors.
		leaf = append([]string{"/bin/sh", "-c", `: > "$0" && exec "$@"`, begunPath(st.e.MarkDir, c.name)}, leaf...)
	}
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

func (st *storm) holding(c *stormClient) bool {
	_, ok := readReport(st.e.MarkDir, c.name)
	return ok
}

// firstWaveArrived reports whether every first-wave client has been seen queued or with its
// command running (a token-only run, which has no ticket, once its command began); seen keeps
// the clients already seen, so a cancelled or killed one stays arrived.
func (st *storm) firstWaveArrived(seen map[int]bool) bool {
	all := true
	for id := 1; id <= firstWave; id++ {
		if seen[id] {
			continue
		}
		c := st.plan(id)
		_, queued := st.ticketID(c)
		_, begun := os.Stat(begunPath(st.e.MarkDir, c.name))
		if queued || st.holding(c) || begun == nil {
			seen[id] = true
			continue
		}
		all = false
	}
	return all
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
	st.mu.Lock()
	st.labels[c.name] = maps.Clone(c.need)
	st.mu.Unlock()
	c.proc = st.e.StartCommand(st.argv(c)...)
	c.proc.name = c.name
	if c.outer == "tok" {
		st.tok.add(c.name, c.need, func(rep ChildReport, reported bool) bool {
			return !c.proc.Exited() || (reported && pidAlive(rep.PID))
		})
	}
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

// cancel waits up to 2 s for c's ticket, then publishes a cancel request for it, through the
// Go CLI by label or through the Python client by id. A ticket admitted meanwhile, or gone,
// makes it a no-op. The cancel command is killed if it has not exited within exitWait, and is
// reaped before the storm ends.
func (st *storm) cancel(ctx context.Context, c *stormClient) {
	id, queued := st.ticketID(c)
	for end := time.Now().Add(2 * time.Second); !queued && time.Now().Before(end); id, queued = st.ticketID(c) {
		if st.holding(c) || !st.after(ctx, c, pollStep) {
			return
		}
	}
	if !queued {
		return
	}
	var p *Proc
	if c.cancelPy {
		p = st.e.StartCommand(st.py, st.pyc, "cancel", id)
	} else {
		p = st.e.Start("cancel", "--label", c.name)
	}
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
	res := stormResult{c: c, code: c.proc.code, stderr: c.proc.Stderr(), admitted: st.holding(c)}
	c.proc.stdout.Close()
	c.proc.stderr.Close()
	if res.admitted {
		st.count("admitted " + c.outer)
		if c.inner != "" {
			st.count("admitted nested")
		}
	}
	st.mu.Lock()
	st.results = append(st.results, res)
	st.mu.Unlock()
}

// resize changes the sizes and counts the change: shrinks, growths, and shrinks that leave more
// tokens locked than the new size.
func (st *storm) resize(ctx context.Context, next map[string]int) bool {
	old := st.s.Sizes()
	locked, err := st.s.Resize(ctx, next)
	if err != nil {
		return false
	}
	for _, res := range slices.Sorted(maps.Keys(next)) {
		n := next[res]
		switch {
		case n < old[res]:
			st.count("shrink")
			if locked[res] > n {
				st.count("shrink below the locked count")
			}
		case n > old[res]:
			st.count("growth")
		}
	}
	return true
}

// sizer changes the configuration: first a shrink of cpu from 4 to 2 once the first wave holds
// (or 5 s passed), then a growth to 6, then random sizes every 1.5 to 4 s.
func (st *storm) sizer(ctx context.Context) {
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
	if ctx.Err() != nil || !st.resize(ctx, map[string]int{"cpu": 2, "io": 2}) {
		return
	}
	shrunk = true
	close(st.shrunk)
	r := rand.New(rand.NewPCG(st.seed, 0))
	if !sleep(time.Second+time.Duration(r.IntN(1500))*time.Millisecond) ||
		!st.resize(ctx, map[string]int{"cpu": 6, "io": 3}) {
		return
	}
	for sleep(1500*time.Millisecond + time.Duration(r.IntN(2500))*time.Millisecond) {
		if !st.resize(ctx, map[string]int{"cpu": 2 + r.IntN(7), "io": 1 + r.IntN(3)}) {
			return
		}
	}
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
		tok: &tokenOnlyRuns{markDir: e.MarkDir}, shrunk: make(chan struct{}),
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
	st.s.TokenOnly = st.tok.grants

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
	wg.Go(func() { st.sizer(ctx) })
	// The first wave arrives before every other client: the slots after it start once each
	// first-wave client is queued or holding (or after 5 s), so that a slow start under load
	// cannot put a first-wave waiter behind a random request that blocks the queue.
	arrived := make(chan struct{})
	go func() {
		defer close(arrived)
		seen := map[int]bool{}
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && ctx.Err() == nil; time.Sleep(pollStep) {
			if st.firstWaveArrived(seen) {
				return
			}
		}
	}()
	for slot := range clients {
		wg.Go(func() {
			delay := time.Duration(slot) * 40 * time.Millisecond // stagger the first arrivals
			if slot >= firstWave {
				select {
				case <-ctx.Done():
					return
				case <-arrived:
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
		refusalExpected := r.c.fate == "cancel" || r.c.timeout > 0
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
	latency, reportedRuns := st.tok.reportLatency()
	summary := fmt.Sprintf("kill-storm seed %d: %d clients in %v, %d samples (%d missed), %d violations; "+
		"token-only start-to-report latency max %v over %d runs (bound %v); events %v; endings %v; load after: %s",
		seed, len(st.results), time.Since(st.started).Round(time.Second), samples, missed, len(violations),
		latency, reportedRuns, st.s.TokenOnlyBound, st.counts, codes, uptime())
	t.Log(summary)
	fmt.Fprintln(os.Stderr, summary)

	// The sampler's token-only bound must cover every start-to-report latency the run saw;
	// one that does not is its own failure here, not a false I2 above.
	if latency >= st.s.TokenOnlyBound {
		t.Errorf("a token-only command took %v from its start marker to its report: the sampler's bound %v is too tight",
			latency, st.s.TokenOnlyBound)
	}

	// The run exercised what it is for: floors on what happened, scaled for -short. The first
	// shrink is deterministic (cpu 4 to 2 while the first wave holds 4), so the floor on a
	// shrink below the locked count checks that the schedule ran, not that chance produced one.
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
	floors := []string{"admitted cli", "admitted lib", "admitted tok", "admitted nested", "SIGKILL of a holder",
		"SIGSTOP", "cancel published", "shrink below the locked count", "growth"}
	if st.py != "" {
		floors = append(floors, "admitted py")
	}
	floorFailed := false
	for _, what := range floors {
		if st.counts[what] < 1 {
			floorFailed = true
			t.Errorf("the storm never exercised %q", what)
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
