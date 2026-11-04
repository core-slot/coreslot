//go:build unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/core-slot/coreslot/internal/config"
	"github.com/core-slot/coreslot/internal/engine"
	"github.com/core-slot/coreslot/internal/spec"
	"github.com/core-slot/coreslot/internal/store"
	"github.com/core-slot/coreslot/internal/wire"
)

// The test binary doubles as the coreslot binary and as the helper commands the tests run:
// CORESLOT_CLI_TEST_HELPER selects a helper, and otherwise CORESLOT_CLI_TEST_AS_MAIN makes it
// coreslot itself, so nested runs started by a command reach the same binary.
const (
	helperEnv   = "CORESLOT_CLI_TEST_HELPER"
	asMainEnv   = "CORESLOT_CLI_TEST_AS_MAIN"
	headroomEnv = "CORESLOT_CLI_TEST_HEADROOM_MIB" // the headroom coreslot reads, in MiB
)

func TestMain(m *testing.M) {
	if h := os.Getenv(helperEnv); h != "" {
		os.Exit(runHelper(h, os.Args[1:]))
	}
	// A helper named in argv[1] (helper:NAME) sees the environment coreslot gave its command,
	// where CORESLOT_CLI_TEST_HELPER would make coreslot itself the helper.
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "helper:") {
		os.Exit(runHelper(strings.TrimPrefix(os.Args[1], "helper:"), os.Args[2:]))
	}
	if os.Getenv(asMainEnv) != "" {
		if v := os.Getenv(headroomEnv); v != "" {
			mib, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				fmt.Fprintln(os.Stderr, headroomEnv+":", err)
				os.Exit(1)
			}
			readMachine = machineWithHeadroom(mib)
		}
		main()
	}
	// A CI runner hands its job steps descriptors that are not close-on-exec. Every process a test
	// starts would inherit them, and coreslot passes them on to its command (PROTOCOL.md section
	// 17), so they would shift the descriptor numbers the tests count on: under a lowered soft
	// limit they can even end above it, and macOS's dyld then aborts the program. Marking them
	// close-on-exec keeps them in this process only.
	if err := closeInheritedOnExec(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot mark the inherited descriptors close-on-exec:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// closeInheritedOnExec marks every open descriptor from 3 on close-on-exec, as listed in fdDir;
// on a system without one it does nothing.
func closeInheritedOnExec() error {
	if fdDir == "" {
		return nil
	}
	ents, err := os.ReadDir(fdDir)
	if err != nil {
		return err
	}
	for _, ent := range ents {
		if fd, err := strconv.Atoi(ent.Name()); err == nil && fd >= 3 {
			unix.CloseOnExec(fd)
		}
	}
	return nil
}

// helperDeadline ends a waiting helper whose test failed before releasing it, so that it can
// never loop on after the test.
const helperDeadline = 2 * time.Minute

// runHelper runs one helper command of the tests.
func runHelper(name string, args []string) int {
	switch name {
	case "getenv":
		fmt.Println(os.Getenv(args[0]))
		return 0
	case "fds":
		// Report the CORESLOT_* environment and every open descriptor from 3 to 600 with its
		// inode, one line each.
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "CORESLOT_") && !strings.HasPrefix(kv, "CORESLOT_CLI_TEST") {
				fmt.Println("env", kv)
			}
		}
		for fd := 3; fd <= 600; fd++ {
			var st unix.Stat_t
			if unix.Fstat(fd, &st) == nil {
				fmt.Println("fd", fd, uint64(st.Dev), uint64(st.Ino))
			}
		}
		return 0
	case "snapshot":
		// Print the live holds of the state directory, one `hold <label> unbudgeted=<bool>` each.
		e, err := openEngine(locate(), false)
		if err != nil {
			fmt.Println("error:", err)
			return 1
		}
		rep, err := e.Snapshot()
		if err != nil {
			fmt.Println("error:", err)
			return 1
		}
		for _, h := range rep.Holds {
			fmt.Printf("hold %s unbudgeted=%v\n", h.Label, h.NeedMiB == 0)
		}
		return 0
	case "inherit-fds":
		// Open args[0] descriptors on the null device, not close-on-exec, and exec args[1:] with
		// them inherited and without this variable, so that a coreslot it starts is coreslot.
		n, err := strconv.Atoi(args[0])
		if err != nil || len(args) < 2 {
			fmt.Fprintln(os.Stderr, "inherit-fds: want a count and a command")
			return 2
		}
		for range n {
			if _, err := unix.Open(os.DevNull, unix.O_RDONLY, 0); err != nil {
				fmt.Fprintln(os.Stderr, "inherit-fds:", err)
				return 1
			}
		}
		path, err := exec.LookPath(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "inherit-fds:", err)
			return 1
		}
		var env []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, helperEnv+"=") {
				env = append(env, kv)
			}
		}
		fmt.Fprintln(os.Stderr, "inherit-fds:", unix.Exec(path, args[1:], env))
		return 1
	case "close-and-wait":
		// Close every descriptor from 3, report it in args[0], keep working until args[1] exists.
		for fd := 3; fd < 1024; fd++ {
			unix.Close(fd)
		}
		if err := os.WriteFile(args[0], nil, 0o600); err != nil {
			return 1
		}
		deadline := time.Now().Add(helperDeadline)
		for {
			if _, err := os.Stat(args[1]); err == nil {
				return 0
			}
			if time.Now().After(deadline) {
				return 3
			}
			time.Sleep(20 * time.Millisecond)
		}
	case "hold-mem":
		// Fill args[0] MiB with incompressible bytes, then hold them for the duration args[1].
		mib, err := strconv.Atoi(args[0])
		if err != nil {
			return 2
		}
		hold, err := time.ParseDuration(args[1])
		if err != nil {
			return 2
		}
		b := make([]byte, mib<<20)
		_, _ = rand.NewChaCha8([32]byte{1}).Read(b)
		time.Sleep(hold)
		runtime.KeepAlive(b)
		return 0
	case "burn":
		// Touch every page of burnBytes, so that they are resident, then spin until this process
		// has used burnCPU of user and system time by its own rusage.
		b := make([]byte, burnBytes)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		for {
			var ru unix.Rusage
			if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
				fmt.Fprintln(os.Stderr, "burn:", err)
				return 1
			}
			if time.Duration(ru.Utime.Nano()+ru.Stime.Nano()) >= burnCPU {
				break
			}
		}
		runtime.KeepAlive(b)
		return 0
	}
	if code, ok := signalHelper(name, args); ok {
		return code
	}
	fmt.Fprintln(os.Stderr, "unknown helper", name)
	return 2
}

// ---------------------------------------------------------------- fixtures

func cliBinary(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// stateDir creates a state directory whose path contains a space, with cfg as its config.toml
// when cfg is not empty.
func stateDir(t *testing.T, cfg string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// machineWithHeadroom reads the machine with its headroom replaced by mib.
func machineWithHeadroom(mib int64) func() (config.Machine, error) {
	return func() (config.Machine, error) {
		m, err := config.ReadMachine()
		m.HeadroomMiB, m.HeadroomOK = mib, true
		return m, err
	}
}

// budgetRoom is the room each budgetDir state directory offers, in MiB.
var budgetRoom sync.Map

// budgetDir creates a state directory with no reserve, whose headroom is roomMiB for every
// coreslot baseEnv starts on it: the room an idle machine offers, which the live holds then use up.
func budgetDir(t *testing.T, roomMiB int64) string {
	t.Helper()
	dir := stateDir(t, "[memory]\nreserve = \"0\"\n")
	budgetRoom.Store(dir, roomMiB)
	return dir
}

// baseEnv is the test's environment without any coreslot context, plus the state directory
// and the switch that makes the test binary coreslot, with the headroom of a budgetDir.
func baseEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CORESLOT_") {
			env = append(env, kv)
		}
	}
	env = append(env, "CORESLOT_STATE_DIR="+dir, asMainEnv+"=1")
	if mib, ok := budgetRoom.Load(dir); ok {
		env = append(env, fmt.Sprintf("%s=%d", headroomEnv, mib))
	}
	return env
}

// cliCmd is a coreslot invocation in its own process group, killed with its group when the
// test ends.
func cliCmd(t *testing.T, env []string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(cliBinary(t), args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		}
	})
	return cmd
}

// helper is the command line of a helper: the test binary with the helper variable set for it
// alone, so that the coreslot running it stays coreslot.
func helper(t *testing.T, name string, args ...string) []string {
	return append([]string{"env", helperEnv + "=" + name, cliBinary(t)}, args...)
}

type result struct {
	code           int
	stdout, stderr string
}

// cli runs coreslot to completion within two minutes.
func cli(t *testing.T, env []string, args ...string) result {
	t.Helper()
	cmd := cliCmd(t, env, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// A descendant in another process group can keep the output pipes open after the kill.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(2 * time.Minute):
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		}
	}()
	_ = cmd.Wait()
	close(done)
	return result{code: exitStatus(cmd.ProcessState), stdout: out.String(), stderr: errb.String()}
}

// locked reports whether some open file description holds the lock of res/<res>/<i>.
func liveHolds(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	for _, name := range entries(t, dir, "holds") {
		f, err := os.Open(filepath.Join(dir, "holds", name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			n++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// otherHolds numbers the holds holdOther writes, so that each has a name of its own.
var otherHolds atomic.Int64

// holdOther publishes a hold reserving needMiB and locks it from the test process until the test
// ends: another client's grant.
func holdOther(t *testing.T, dir string, needMiB int64) {
	t.Helper()
	f, err := os.Open(deadHold(t, dir, needMiB))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
}

// deadHold writes a hold reserving needMiB that nobody locks, as a client killed before it
// removed its hold leaves it, and returns its path.
func deadHold(t *testing.T, dir string, needMiB int64) string {
	t.Helper()
	h := wire.Hold{Label: "other", PID: 4000 + int(otherHolds.Add(1)), StartNS: 1, NeedMiB: needMiB}
	body, err := wire.Encode(h)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "holds"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "holds", wire.HoldName(h.StartNS, h.PID, 1, h.Label))
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// entries lists a directory of the layout without temporaries; a missing one is empty.
func entries(t *testing.T, dir, sub string) []string {
	t.Helper()
	des, err := os.ReadDir(filepath.Join(dir, sub))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		if !strings.HasPrefix(de.Name(), ".tmp-") {
			names = append(names, de.Name())
		}
	}
	return names
}

// runLog reads log/runs.jsonl as one map per line.
func runLog(t *testing.T, dir string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "log", "runs.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("run log line %q: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	return lines
}

// waitFor polls cond every 20 ms for up to d.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v (load: %s)", what, d, uptime())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func uptime() string {
	out, _ := exec.Command("uptime").Output()
	return strings.TrimSpace(string(out))
}

// envOf parses `env K=V` lines of the fds helper.
func envOf(out string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if kv, ok := strings.CutPrefix(line, "env "); ok {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
	}
	return env
}

// ---------------------------------------------------------------- pure functions

// The CLI's own state errors keep their cause, as the store's do: a location that failed and a
// state directory that cannot be created both still match the errno behind them.
func TestOpenEngineStateErrorWrapsItsCause(t *testing.T) {
	cause := &os.PathError{Op: "open", Path: "/etc/passwd", Err: syscall.EACCES}
	if _, err := openEngine(location{err: cause}, true); !errors.Is(err, syscall.EACCES) {
		t.Errorf("openEngine of a failed location: %v (%T), want it to wrap EACCES", err, err)
	}
	if os.Geteuid() == 0 {
		return // root ignores directory permissions
	}
	parent := filepath.Join(t.TempDir(), "read-only")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o700) })
	_, err := openEngine(location{dir: filepath.Join(parent, "state")}, false)
	var se *stateError
	if !errors.As(err, &se) || !errors.Is(err, syscall.EACCES) {
		t.Errorf("openEngine under a read-only parent: %v (%T), want a *stateError wrapping EACCES", err, err)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		code     int
		failOpen bool
	}{
		{"usage", &spec.UsageError{Msg: "bad need"}, 64, false},
		{"refusal", &refusalError{Msg: "cancelled"}, 75, false},
		{"refusal code", &refusalError{Msg: "timeout", Code: 99}, 99, false},
		{"cli state error", &stateError{Reason: "unusable"}, 0, true},
		{"cli state error with a cause", &stateError{Reason: "x", Err: &os.PathError{Op: "open", Path: "/s", Err: syscall.EACCES}}, 0, true},
		{"strict turns a state error into a refusal", strictRefusal(&store.StateError{Reason: "x"}, 75), 75, false},
		{"strict with --refusal-code", strictRefusal(&stateError{Reason: "x"}, 99), 99, false},
		{"interrupted", &interruptError{Sig: syscall.SIGINT}, 130, false},
		{"engine refusal", fmt.Errorf("%w: format 2", engine.ErrRefused), 75, false},
		{"not found", &execError{Err: &exec.Error{Name: "nope", Err: exec.ErrNotFound}}, 127, false},
		{"missing path", &execError{Err: &os.PathError{Op: "fork/exec", Path: "/nope", Err: syscall.ENOENT}}, 127, false},
		{"not executable", &execError{Err: &os.PathError{Op: "fork/exec", Path: "/etc", Err: syscall.EACCES}}, 126, false},
		{"exec failure of another kind", &execError{Err: &os.PathError{Op: "fork/exec", Path: "/bin/sh", Err: syscall.E2BIG}}, 70, false},
		// Only the run's own exec gives 126 and 127: the same errnos elsewhere are I/O on the state.
		{"state entry missing", &os.PathError{Op: "open", Path: "/s/queue", Err: syscall.ENOENT}, 0, true},
		{"state entry unreadable", &os.PathError{Op: "open", Path: "/s/queue", Err: syscall.EACCES}, 0, true},
		{"store state error", fmt.Errorf("list: %w", &store.StateError{Reason: "x"}), 0, true},
		{"untyped error", errors.New("boom"), 0, true},
		{"cancelled context", context.Canceled, 70, false},
		{"no such ticket", engine.ErrNoTicket, 70, false},
	}
	for _, c := range cases {
		code, failOpen := classify(c.err)
		if code != c.code || failOpen != c.failOpen {
			t.Errorf("%s: classify = (%d, %v), want (%d, %v)", c.name, code, failOpen, c.code, c.failOpen)
		}
	}
}

func TestWindowsNotice(t *testing.T) {
	if got := windowsNotice(config.Config{WindowsWarning: true}); !strings.Contains(got, "without") {
		t.Errorf("warning on: %q", got)
	}
	if got := windowsNotice(config.Config{WindowsWarning: false}); got != "" {
		t.Errorf("warning off: %q, want none", got)
	}
}

func TestRunUsageErrors(t *testing.T) {
	dir := stateDir(t, "")
	for _, args := range [][]string{
		{"run", "--restart=yes", "--mem", "0", "--", "true"},
		{"run", "--restart=yes", "--mem", "lots", "--", "true"},
		{"run", "--restart=yes", "--mem", "1G", "--mem", "x", "--", "true"},
		{"run", "--restart=yes", "--bogus", "--", "true"},
		{"run"},
		{"run", "--restart=yes", "--on-timeout", "maybe", "--", "true"},
	} {
		if r := cli(t, baseEnv(dir), args...); r.code != 64 {
			t.Errorf("%q: exit %d, want 64 (stderr %q)", args, r.code, r.stderr)
		}
	}
	if q := entries(t, dir, "queue"); len(q) != 0 {
		t.Errorf("a usage error queued: %q", q)
	}
}

// A run that could be stopped refuses without --restart, with a usage error that says what to
// pick, before it queues; a run whose command shares coreslot's group (--same-group, a run nested
// inside another's grant) or whose label is no_restart needs none. Mutation: dropping the check
// runs the first command; requiring it everywhere refuses the others.
func TestRestartRequired(t *testing.T) {
	dir := stateDir(t, "")
	r := cli(t, baseEnv(dir), "run", "--mem", "64M", "--", "true")
	if r.code != 64 || !strings.Contains(r.stderr, "--restart is required") || !strings.Contains(r.stderr, "When unsure, --restart=no") {
		t.Errorf("without --restart: exit %d, stderr %q; want 64 and what to pick", r.code, r.stderr)
	}
	if q := entries(t, dir, "queue"); len(q) != 0 {
		t.Errorf("the refused run queued: %q", q)
	}
	writeNeeds(t, dir, "[needs.keep]\nno_restart = true\n")
	for _, args := range [][]string{
		{"run", "--same-group", "--mem", "64M", "--", "true"},
		{"run", "--label", "keep", "--mem", "64M", "--", "true"},
		{"run", "--restart=yes", "--mem", "64M", "--", cliBinary(t), "run", "--mem", "64M", "--", "true"},
		{"run", "--restart=no", "--mem", "64M", "--", "true"},
	} {
		if r := cli(t, baseEnv(dir), args...); r.code != 0 {
			t.Errorf("%q: exit %d, stderr %q; want 0", args, r.code, r.stderr)
		}
	}
}

// ---------------------------------------------------------------- runs

func TestRunPassesExitStatus(t *testing.T) {
	dir := stateDir(t, "")
	if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", "sh", "-c", "exit 3"); r.code != 3 {
		t.Errorf("exit 3: got %d (stderr %q)", r.code, r.stderr)
	}
	if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", "sh", "-c", "kill -KILL $$"); r.code != 137 {
		t.Errorf("SIGKILL: got %d, want 137 (stderr %q)", r.code, r.stderr)
	}
}

// A command's own exit 75 is its status, with no marker line. Mutation: reading 75 as a
// give-up prints the marker.
func TestOwnExit75PassesThrough(t *testing.T) {
	dir := stateDir(t, "")
	if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", "sh", "-c", "exit 75"); r.code != 75 || r.stderr != "" {
		t.Errorf("exit 75: got %d, stderr %q; want 75 and nothing said", r.code, r.stderr)
	}
}

// A SIGKILL coreslot did not send is reported, and the command is not restarted. Mutation:
// counting every SIGKILL as a stop restarts it.
func TestOutsideSigkillNotRestarted(t *testing.T) {
	dir := stateDir(t, "")
	marks := filepath.Join(t.TempDir(), "starts")
	r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", "sh", "-c", `echo >>"$0"; kill -KILL $$`, marks)
	if want := "ended by SIGKILL from outside coreslot; not restarted"; r.code != 137 || !strings.Contains(r.stderr, want) {
		t.Errorf("got %d, stderr %q; want 137 and %q", r.code, r.stderr, want)
	}
	if data, err := os.ReadFile(marks); err != nil || len(data) != 1 {
		t.Errorf("the command started %d times (%v), want once", len(data), err)
	}
}

// A command's own exit 137 is its status, not a SIGKILL: nothing is said of one. Mutation: reading
// the status 137 as the kill reports a SIGKILL from outside coreslot.
func TestOwnExit137IsNoKill(t *testing.T) {
	dir := stateDir(t, "")
	if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", "sh", "-c", "exit 137"); r.code != 137 || r.stderr != "" {
		t.Errorf("exit 137: got %d, stderr %q; want 137 and nothing said", r.code, r.stderr)
	}
}

// A stopped command comes back with the larger of its peak and its need. Mutation: keeping the
// need gives 1 GiB.
func TestRestartNeedIsPeak(t *testing.T) {
	if got := restartNeed(3<<20, 1024); got != 3072 {
		t.Errorf("restartNeed(3 GiB peak, 1 GiB need) = %d MiB, want 3072", got)
	}
	if got := restartNeed(512<<10, 1024); got != 1024 {
		t.Errorf("restartNeed(512 MiB peak, 1 GiB need) = %d MiB, want 1024", got)
	}
}

func TestRunMissingCommandFreesEverything(t *testing.T) {
	dir := budgetDir(t, 512)
	for _, cmd := range []string{"coreslot-no-such-command", filepath.Join(dir, "missing")} {
		r := cli(t, baseEnv(dir), "run", "--restart=yes", "--mem", "256M", "--", cmd)
		if r.code != 127 {
			t.Errorf("%s: exit %d, want 127 (stderr %q)", cmd, r.code, r.stderr)
		}
		if h := entries(t, dir, "holds"); len(h) != 0 {
			t.Errorf("%s: holds left: %q", cmd, h)
		}
		if n := liveHolds(t, dir); n != 0 {
			t.Errorf("%s: %d holds still live", cmd, n)
		}
	}
	notExec := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", notExec); r.code != 126 {
		t.Errorf("not executable: exit %d, want 126 (stderr %q)", r.code, r.stderr)
	}
}

func TestCtrlCWhileWaitingExits130(t *testing.T) {
	dir := budgetDir(t, 128)
	if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	holdOther(t, dir, 128)
	cmd := cliCmd(t, baseEnv(dir), "run", "--restart=yes", "--", "true")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the ticket", func() bool { return len(entries(t, dir, "queue")) == 1 })
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if got := exitStatus(cmd.ProcessState); got != 130 {
		t.Fatalf("exit %d, want 130 (stderr %q)", got, errb.String())
	}
	for _, name := range entries(t, dir, "queue") {
		f, err := os.Open(filepath.Join(dir, "queue", name))
		if err != nil {
			continue
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Errorf("ticket %s is still live after the interrupt", name)
		}
		f.Close()
	}
}

func TestNoWaitAdmitsAtOnce(t *testing.T) {
	dir := budgetDir(t, 512)
	r := cli(t, baseEnv(dir), "run", "--restart=yes", "--no-wait", "--mem", "384M", "--", "sh", "-c", "echo $CORESLOT_GRANTED_MEM")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "384" {
		t.Errorf("exit %d stdout %q stderr %q, want 0 and a grant of 384 MiB", r.code, r.stdout, r.stderr)
	}
	budgetFree(t, dir)
}

func TestNoWaitRefusesWithin2s(t *testing.T) {
	dir := budgetDir(t, 256)
	if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}

	t.Run("section held", func(t *testing.T) {
		f, err := os.OpenFile(filepath.Join(dir, "admit.lock"), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		r := cli(t, baseEnv(dir), "run", "--restart=yes", "--no-wait", "--", "true")
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("refused after %v, want within 2 s (load: %s)", took, uptime())
		}
		if r.code != 75 || !strings.Contains(r.stderr, "--no-wait") {
			t.Errorf("exit %d stderr %q, want 75 and a --no-wait diagnostic", r.code, r.stderr)
		}
	})

	t.Run("budget full", func(t *testing.T) {
		holdOther(t, dir, 256)
		r := cli(t, baseEnv(dir), "run", "--restart=yes", "--no-wait", "--refusal-code", "9", "--", "true")
		if r.code != 9 {
			t.Errorf("exit %d stderr %q, want the refusal code 9", r.code, r.stderr)
		}
	})
}

func TestTimeoutRunRecordsUnbudgeted(t *testing.T) {
	dir := budgetDir(t, 128)
	if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	holdOther(t, dir, 128)
	before := len(runLog(t, dir))

	r := cli(t, baseEnv(dir), "run", "--restart=yes", "--mem", "128M", "--timeout", "1s", "--", "true")
	if r.code != 75 {
		t.Fatalf("--on-timeout defaults to fail: exit %d, want 75 (stderr %q)", r.code, r.stderr)
	}

	r = cli(t, append(baseEnv(dir), "SNAPSHOT="+strings.Join(helper(t, "snapshot"), " ")), "run", "--restart=yes", "--mem", "128M",
		"--label", "late", "--timeout", "1s", "--on-timeout", "run", "--", "sh", "-c",
		`echo "granted=$CORESLOT_GRANTED_MEM unbudgeted=$CORESLOT_UNBUDGETED"; $SNAPSHOT`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if got := strings.Split(strings.TrimSpace(r.stdout), "\n")[0]; got != "granted=128 unbudgeted=timeout" {
		t.Errorf("command saw %q", got)
	}
	if !strings.Contains(r.stdout, "hold late unbudgeted=true") {
		t.Errorf("the snapshot taken while the command ran shows no unbudgeted hold: %q", r.stdout)
	}
	lines := runLog(t, dir)
	if len(lines) != before+1 {
		t.Fatalf("run log has %d lines, want %d", len(lines), before+1)
	}
	last := lines[len(lines)-1]
	if last["fail_open"] != "timeout" {
		t.Errorf("run log line %v: want the reason timeout", last)
	}
	if h := entries(t, dir, "holds"); len(h) != 1 { // the other client's only
		t.Errorf("the unbudgeted hold outlived the run: %q", h)
	}
	if _, err := os.Stat(filepath.Join(dir, "degraded")); err == nil {
		t.Error("a timeout recorded degraded mode")
	}
}

func TestFailOpenExportsUnbudgeted(t *testing.T) {
	show := `echo "granted=$CORESLOT_GRANTED_MEM unbudgeted=$CORESLOT_UNBUDGETED"`

	t.Run("unusable directory", func(t *testing.T) {
		dir := stateDir(t, "")
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		r := cli(t, baseEnv(dir), "run", "--restart=yes", "--mem", "384M", "--", "sh", "-c", show)
		if r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		out := strings.TrimSpace(r.stdout)
		if !strings.HasPrefix(out, "granted=384 unbudgeted=") || out == "granted=384 unbudgeted=" {
			t.Errorf("command saw %q, want granted=384 and a reason", out)
		}
		if !strings.Contains(r.stderr, "state error") || !strings.Contains(r.stderr, "nothing was written") {
			t.Errorf("stderr %q: want the diagnostic and that nothing was recorded", r.stderr)
		}
		if names := entries(t, dir, ""); len(names) != 0 {
			t.Errorf("wrote into an unusable directory: %q", names)
		}
	})

	t.Run("invalid configuration", func(t *testing.T) {
		dir := stateDir(t, "[memory]\nreserve = \"bogus\"\n")
		r := cli(t, baseEnv(dir), "run", "--restart=yes", "--mem", "256M", "--", "sh", "-c",
			show+`; ls "$CORESLOT_STATE_DIR/holds"`)
		if r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		if !strings.Contains(r.stdout, "granted=256 unbudgeted=configuration") {
			t.Errorf("command saw %q", r.stdout)
		}
		if strings.Count(r.stdout, "\n") < 2 {
			t.Errorf("no unbudgeted hold was published while the command ran: %q", r.stdout)
		}
		if h := entries(t, dir, "holds"); len(h) != 0 {
			t.Errorf("the unbudgeted hold was not removed after the run: %q", h)
		}
		b, err := os.ReadFile(filepath.Join(dir, "degraded"))
		if err != nil || !strings.Contains(string(b), `"count":1`) {
			t.Errorf("degraded = %q, %v; want a record with count 1", b, err)
		}
		lines := runLog(t, dir)
		if len(lines) != 1 || lines[0]["fail_open"] == "" {
			t.Errorf("run log %v: want one line with the fail-open reason", lines)
		}
	})

	t.Run("strict refuses", func(t *testing.T) {
		dir := stateDir(t, "[memory]\nreserve = \"bogus\"\n")
		marker := filepath.Join(t.TempDir(), "ran")
		r := cli(t, baseEnv(dir), "run", "--restart=yes", "--strict", "--", "touch", marker)
		if r.code != 75 {
			t.Errorf("exit %d, want 75 (stderr %q)", r.code, r.stderr)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("the command ran under --strict")
		}
	})
}

func TestRunAppendsOneLogLine(t *testing.T) {
	dir := budgetDir(t, 512)
	env := append(baseEnv(dir), "CORESLOT_TEST_SECRET=env-secret-value")
	r := cli(t, env, "run", "--restart=yes", "--mem", "384M", "--label", "gate:unit", "--", "sh", "-c", "exit 0", "argv-secret-value")
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	b, err := os.ReadFile(filepath.Join(dir, "log", "runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(b, []byte("\n")) != 1 {
		t.Fatalf("run log %q: want exactly one line", b)
	}
	for _, secret := range []string{"argv-secret-value", "env-secret-value", "exit 0"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("the run log contains %q: %s", secret, b)
		}
	}
	line := runLog(t, dir)[0]
	if line["label"] != "gate:unit" || line["exit"] != float64(0) {
		t.Errorf("run log line %v", line)
	}
}

// What the burn helper makes resident and how much user and system time it uses.
const (
	burnBytes = 64 << 20
	burnCPU   = 50 * time.Millisecond
)

// checkUsage asserts that a run-log line carries the burn helper's usage (PROTOCOL.md section
// 20): user and system seconds adding up to at least burnCPU, and a memory peak in KiB of at least
// half of burnBytes and below 10 GiB. A command this short ends before the sampler's first
// reading, so the peak is the rusage's, and the upper bound catches ru_maxrss written in bytes,
// which is macOS's unit: 64 MiB in bytes is more than 10 GiB in KiB.
func checkUsage(t *testing.T, what string, line map[string]any) {
	t.Helper()
	user, uok := line["user_s"].(float64)
	sys, sok := line["sys_s"].(float64)
	if !uok || !sok || user < 0 || sys < 0 || user+sys < burnCPU.Seconds() {
		t.Errorf("%s: user_s %v, sys_s %v; want numbers adding up to at least %v",
			what, line["user_s"], line["sys_s"], burnCPU.Seconds())
	}
	rss, ok := line["mem_peak_kib"].(float64)
	if !ok || rss != float64(int64(rss)) || rss < burnBytes/2/1024 || rss >= 10<<20 {
		t.Errorf("%s: mem_peak_kib %v; want an integer from %d KiB to below 10 GiB",
			what, line["mem_peak_kib"], burnBytes/2/1024)
	}
}

// Every run that started its command records the command's user and system seconds and its peak
// resident set size from its wait4 rusage, which also covers the descendants the command waited
// for: an ordinary run, a fail-open run, and a pass-through, whose parent's line covers it too.
//
// Mutation: drop the recordUsage call in runChild (the keys are absent), or return ru_maxrss
// unconverted in sys_darwin.go's maxRSSKiB (bytes, above 10 GiB in KiB).
func TestRunLogRecordsUsage(t *testing.T) {
	burn := helper(t, "burn")
	t.Run("command", func(t *testing.T) {
		dir := budgetDir(t, 512)
		r := cli(t, baseEnv(dir), append([]string{"run", "--restart=yes", "--mem", "128M", "--"}, burn...)...)
		if r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		lines := runLog(t, dir)
		if len(lines) != 1 {
			t.Fatalf("run log %v: want one line", lines)
		}
		checkUsage(t, "the command", lines[0])
	})
	t.Run("waited-for descendant", func(t *testing.T) {
		dir := budgetDir(t, 512)
		args := append([]string{"run", "--restart=yes", "--mem", "128M", "--", "sh", "-c", `"$@"; :`, "sh"}, burn...)
		r := cli(t, baseEnv(dir), args...)
		if r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		lines := runLog(t, dir)
		if len(lines) != 1 {
			t.Fatalf("run log %v: want one line", lines)
		}
		checkUsage(t, "the shell that waited for it", lines[0])
	})
	t.Run("fail-open", func(t *testing.T) {
		dir := stateDir(t, "[memory]\nreserve = \"bogus\"\n")
		r := cli(t, baseEnv(dir), append([]string{"run", "--restart=yes", "--mem", "128M", "--"}, burn...)...)
		if r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		lines := runLog(t, dir)
		if len(lines) != 1 || lines[0]["fail_open"] == "" {
			t.Fatalf("run log %v: want one fail-open line", lines)
		}
		checkUsage(t, "the fail-open run", lines[0])
	})
	t.Run("pass-through", func(t *testing.T) {
		dir := budgetDir(t, 512)
		args := append([]string{"run", "--restart=yes", "--mem", "256M", "--label", "outer", "--", cliBinary(t),
			"run", "--restart=yes", "--mem", "128M", "--label", "inner", "--"}, burn...)
		r := cli(t, baseEnv(dir), args...)
		if r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		lines := runLog(t, dir)
		if len(lines) != 1 || lines[0]["label"] != "outer" {
			t.Fatalf("run log %v: want the outer run's line alone", lines)
		}
		checkUsage(t, "the run around the pass-through", lines[0])
	})
}

// A run records the peak of its whole command tree, which wait4's rusage cannot see while the
// shell waits for two children at once: two helpers of 200 MiB each, held for 2.5 s, so that
// the sampler reads them both at 1 and 2 s. Mutation: drop the sampler from runChild, and the
// peak is the larger child's max_rss, about 200 MiB.
func TestRunLogsTreePeak(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no process-tree sampler on " + runtime.GOOS)
	}
	dir := stateDir(t, "")
	args := append([]string{"run", "--restart=yes", "--label", "t", "--", "sh", "-c", `"$@" & "$@" & wait`, "sh"},
		helper(t, "hold-mem", "200", "2500ms")...)
	r := cli(t, baseEnv(dir), args...)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	lines := runLog(t, dir)
	if len(lines) != 1 {
		t.Fatalf("run log %v: want one line", lines)
	}
	kib, ok := lines[0]["mem_peak_kib"].(float64)
	if !ok || kib < 360<<10 {
		t.Errorf("mem_peak_kib %v; want at least %d KiB", lines[0]["mem_peak_kib"], 360<<10)
	}
}

// A command that could not start has no rusage: its run still writes a line, without the usage
// keys rather than with zeros.
//
// Mutation: in recordUsage, record zeros when ps is nil.
func TestRunLogOmitsUsageWhenTheCommandDidNotStart(t *testing.T) {
	dir := budgetDir(t, 512)
	notExec := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		label, cmd string
		code       int
	}{{"not-found", "coreslot-no-such-command", 127}, {"not-executable", notExec, 126}} {
		r := cli(t, baseEnv(dir), "run", "--restart=yes", "--mem", "128M", "--label", c.label, "--", c.cmd)
		if r.code != c.code {
			t.Errorf("%s: exit %d, want %d (stderr %q)", c.cmd, r.code, c.code, r.stderr)
		}
	}
	lines := runLog(t, dir)
	if len(lines) != 2 {
		t.Fatalf("run log %v: want a line for each run", lines)
	}
	for _, line := range lines {
		for _, key := range []string{"user_s", "sys_s", "mem_peak_kib"} {
			if v, ok := line[key]; ok {
				t.Errorf("%v (exit %v): %s is %v; want it absent", line["label"], line["exit"], key, v)
			}
		}
	}
}

// A need above what the machine has less the reserve is said once, each size in its unit, with that
// bound (0 under a reserve above the machine), and runs alone when nothing else holds. Mutations:
// saying it for every wait line turns the count red; printing every size in GiB says "0.1 GiB" for
// 128 MiB; printing the machine's memory says GiB for the bound under the 1000 GiB reserve.
func TestUnsatisfiableNeedSaidOnce(t *testing.T) {
	big := stateDir(t, "[memory]\nreserve = \"1000G\"\n") // more than the machine has
	budgetRoom.Store(big, int64(512))
	for dir, mem := range map[string]string{budgetDir(t, 512): "1000G", big: "128M"} {
		r := cli(t, baseEnv(dir), "run", "--restart=yes", "--mem", mem, "--", "true")
		if r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		re := regexp.MustCompile(`coreslot: needs (1000 GiB, more than the \d+(\.\d)? GiB|128 MiB, more than the 0 MiB) ` +
			`the machine has above its reserve; runs only alone\n`)
		if got := re.FindAllString(r.stderr, -1); len(got) != 1 {
			t.Errorf("--mem %s: stderr %q, want the notice once", mem, r.stderr)
		}
	}
}

func TestCLIKeepsCopiesUntilChildExits(t *testing.T) {
	dir := budgetDir(t, 256)
	tmp := t.TempDir()
	closed, release := filepath.Join(tmp, "closed"), filepath.Join(tmp, "release")
	args := append([]string{"run", "--restart=yes", "--mem", "256M", "--"}, helper(t, "close-and-wait", closed, release)...)
	cmd := cliCmd(t, baseEnv(dir), args...)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the command closing its descriptors", func() bool {
		_, err := os.Stat(closed)
		return err == nil
	})
	time.Sleep(200 * time.Millisecond)
	if n := liveHolds(t, dir); n != 1 {
		t.Errorf("%d live holds while the command still runs, want its 1", n)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if got := exitStatus(cmd.ProcessState); got != 0 {
		t.Fatalf("exit %d (stderr %q)", got, errb.String())
	}
	if n := liveHolds(t, dir); n != 0 {
		t.Errorf("%d live holds after the command exited, want 0", n)
	}
}

// ---------------------------------------------------------------- the waiting line

type fakeStepper struct{ calls int }

// due is a non-head waiter's: its next election is a second away.
func (s *fakeStepper) due() time.Duration { return time.Second }

func (s *fakeStepper) step(context.Context) (engine.StepResult, error) {
	s.calls++
	return engine.Waiting{Position: 2}, nil
}

// fakeClock advances only when the loop sleeps; it ends the context once limit has passed.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Duration
	limit  time.Duration
	cancel context.CancelFunc
}

func (c *fakeClock) Now() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now += d
	over := c.now > c.limit
	c.mu.Unlock()
	if over {
		c.cancel()
	}
	return ctx.Err()
}

// The waiting line comes at the start of a wait and every 30 s after, with why the run waits; the
// first alone says what to do about a tool's time limit.
func TestWaitingLineEvery30s(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := &fakeClock{limit: 95 * time.Second, cancel: cancel}
	s := &fakeStepper{}
	var out bytes.Buffer
	snapshots := 0
	r := &run{o: runOptions{need: 1024}, why: func() (string, []engine.HolderInfo) {
		snapshots++
		return "needs 1 GiB, 512 MiB free", []engine.HolderInfo{{Label: "gate", PID: 4711, NeedMiB: 1536,
			StartNS: time.Now().Add(-192 * time.Second).UnixNano()}}
	}}
	_, err := waitLoop(ctx, s, clk, &out, r.waitingLine)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitLoop = %v, want the context's end", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("%d waiting lines in 95 s, want 4 (at 0, 30, 60 and 90 s):\n%s", len(lines), out.String())
	}
	const advice = "; if your tool has a time limit, run this in the background or raise the limit"
	for i, want := range []string{"(0s)", "(30s)", "(1m00s)", "(1m30s)"} {
		prefix := "coreslot: waiting for mem 1 GiB: position 2 " + want + "; needs 1 GiB, 512 MiB free; holders: gate pid 4711 mem 1.5 GiB (3m1"
		if !strings.HasPrefix(lines[i], prefix) || strings.HasSuffix(lines[i], advice) != (i == 0) {
			t.Errorf("line %d = %q", i, lines[i])
		}
	}
	if snapshots != 4 {
		t.Errorf("%d snapshots in 95 s, want 4: one for each waiting line", snapshots)
	}
	if s.calls != 96 {
		t.Errorf("%d steps in 95 s, want 96: one at the start and one each time the next is due (1 s)", s.calls)
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)

// A --timeout that expires while another process holds the section ends the run in time, with
// no ticket left behind, and says why: the enqueue deadline's refusal prints the same notice as
// a timeout that expires in the queue.
func TestTimeoutWhileSectionHeld(t *testing.T) {
	dir := stateDir(t, "")
	if r := cli(t, baseEnv(dir), "run", "--restart=yes", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	f, err := os.OpenFile(filepath.Join(dir, "admit.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := cli(t, baseEnv(dir), "run", "--restart=yes", "--timeout", "1s", "--", "true")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("refused after %v, want soon after the 1 s timeout (load: %s)", took, uptime())
	}
	if r.code != 75 || !strings.Contains(r.stderr, "not admitted within 1s (--timeout); refusing") {
		t.Errorf("exit %d (stderr %q), want 75 and the timeout refusal notice", r.code, r.stderr)
	}
	if q := entries(t, dir, "queue"); len(q) != 0 {
		t.Errorf("tickets left behind: %q", q)
	}
}

// A run's enqueue is bounded by its context and leaves no goroutine behind.
func TestEnqueueBoundedLeavesNothing(t *testing.T) {
	dir := stateDir(t, "")
	t.Setenv("CORESLOT_STATE_DIR", dir)
	e, err := openEngine(locate(), false)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "admit.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	installStops() // its goroutines live as long as the process
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	tk, err := e.EnqueueContext(ctx, spec.Request{NeedMiB: 128})
	if !errors.Is(err, context.DeadlineExceeded) || tk != nil {
		t.Fatalf("enqueue = %v, %v; want the deadline and no ticket", tk, err)
	}
	f.Close() // the section is free now: an abandoned Enqueue would publish a ticket
	time.Sleep(500 * time.Millisecond)
	if n := runtime.NumGoroutine(); n > baseline {
		t.Errorf("%d goroutines after enqueue returned, %d before", n, baseline)
	}
	if q := entries(t, dir, "queue"); len(q) != 0 {
		t.Errorf("a ticket was published after enqueue returned: %q", q)
	}
}

// bigRunLog is a state directory whose FORMAT holds format and whose run log is at the rotation
// size, so that the next append rotates first.
func bigRunLog(t *testing.T, format string) (*store.Store, string) {
	t.Helper()
	dir := stateDir(t, "")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "FORMAT"), []byte(format), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "log"), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "log", "runs.jsonl"), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(store.RotateRunLogAt); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return st, dir
}

// Only a FORMAT the rotation section does not understand, or a malformed one, stops the append
// (PROTOCOL.md section 14): the line is not written into a directory of an unknown format.
func TestRunLogSkippedWhenRotationFindsUnknownFormat(t *testing.T) {
	for _, format := range []string{"coreslot-protocol 3\n", "coreslot-protocol x\n"} {
		st, dir := bigRunLog(t, format)
		var notices []string
		notice := func(f string, args ...any) { notices = append(notices, fmt.Sprintf(f, args...)) }
		if err := appendRunLog(st, store.RunRecord{Label: "x"}, notice); err == nil {
			t.Errorf("FORMAT %q: appendRunLog: want an error", format)
		}
		// The rotation section's refusal stops the append by itself, not as a failed rotation
		// left to the re-read before the append: a FORMAT changed back in between would not
		// bring the line back.
		if len(notices) != 0 {
			t.Errorf("FORMAT %q: notices %q, want none", format, notices)
		}
		if fi, _ := os.Stat(filepath.Join(dir, "log", "runs.jsonl")); fi.Size() != store.RotateRunLogAt {
			t.Errorf("FORMAT %q: the run log grew to %d bytes", format, fi.Size())
		}
		if _, err := os.Stat(filepath.Join(dir, "log", "runs.1.jsonl")); err == nil {
			t.Errorf("FORMAT %q: the run log was rotated", format)
		}
	}
}

// FORMAT is re-read immediately before the run-log line is appended (PROTOCOL.md section 20):
// a run whose command replaces FORMAT with one this client does not implement, or with a
// malformed one, writes no line and says so on stderr, though FORMAT was understood when it was
// admitted.
func TestRunLogSkippedWhenFormatChangesDuringRun(t *testing.T) {
	for name, c := range map[string]struct{ format, notice string }{
		"unknown":   {"coreslot-protocol 3\n", "FORMAT is missing or names a format this client does not implement"},
		"malformed": {"coreslot-protocol x\n", "malformed FORMAT"},
	} {
		format := c.format
		t.Run(name, func(t *testing.T) {
			dir := stateDir(t, "")
			env := append(baseEnv(dir), "S="+dir, "F="+format)
			r := cli(t, env, "run", "--restart=yes", "--label", "changed", "--", "sh", "-c", `printf %s "$F" > "$S/FORMAT"`)
			if r.code != 0 {
				t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
			}
			if lines := runLog(t, dir); len(lines) != 0 {
				t.Errorf("a run-log line was written after FORMAT became %q: %v", format, lines)
			}
			if !strings.Contains(r.stderr, "the run log was not written: ") || !strings.Contains(r.stderr, c.notice) {
				t.Errorf("stderr %q: want a notice that the run log was not written naming %q", r.stderr, c.notice)
			}
		})
	}
}

// At the re-read just before the append, a FORMAT that is missing or cannot be read stops the
// append too (PROTOCOL.md section 20): appendRunLog returns the error and writes nothing.
func TestRunLogSkippedWhenFormatMissingOrUnreadable(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, dir string){
		"missing": func(t *testing.T, dir string) {},
		"a directory": func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "FORMAT"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := stateDir(t, "")
			st, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			setup(t, dir)
			notice := func(f string, args ...any) { t.Errorf("notice: "+f, args...) }
			err = appendRunLog(st, store.RunRecord{Label: "x"}, notice)
			if err == nil || (name == "missing") != errors.Is(err, errFormatChanged) {
				t.Errorf("appendRunLog: %v; want errFormatChanged for a missing FORMAT, the read error otherwise", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "log", "runs.jsonl")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the run log exists (%v)", err)
			}
		})
	}
}

// A command that removes FORMAT does not cost its run the line: the run's release section,
// which comes before the append, writes the missing FORMAT again (format 2), and the re-read then
// understands it.
func TestRunLogAfterTheCommandRemovesFormat(t *testing.T) {
	dir := stateDir(t, "")
	env := append(baseEnv(dir), "S="+dir)
	r := cli(t, env, "run", "--restart=yes", "--label", "removed", "--", "sh", "-c", `rm "$S/FORMAT"`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "FORMAT")); err != nil || string(b) != "coreslot-protocol 2\n" {
		t.Errorf("FORMAT after the run: %q, %v; want format 2 written again", b, err)
	}
	if lines := runLog(t, dir); len(lines) != 1 || lines[0]["label"] != "removed" {
		t.Errorf("run log %v: want the run's line", lines)
	}
	if strings.Contains(r.stderr, "the run log was not written") {
		t.Errorf("stderr %q: want no notice", r.stderr)
	}
}

// Any other rotation failure costs the rotation, never the line: one notice, then the append.
func TestRunLogAppendedWhenRotationFails(t *testing.T) {
	st, dir := bigRunLog(t, "coreslot-protocol 2\n")
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "log", "runs.1.jsonl")); err != nil {
		t.Fatal(err)
	}
	var notices []string
	notice := func(f string, args ...any) { notices = append(notices, fmt.Sprintf(f, args...)) }
	if err := appendRunLog(st, store.RunRecord{Label: "kept"}, notice); err != nil {
		t.Fatalf("appendRunLog: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "log", "runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) <= store.RotateRunLogAt || !bytes.Contains(b[store.RotateRunLogAt:], []byte(`"label":"kept"`)) {
		t.Errorf("the line was not appended after the failed rotation (%d bytes)", len(b))
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "rotat") {
		t.Errorf("notices %q, want one about the rotation", notices)
	}
}

// A section entered only to rotate the run log reconciles first when reconciliation is due
// (PROTOCOL.md section 10.4): a dead ticket is removed and gen records the reconciliation.
func TestRunLogRotationReconcilesWhenDue(t *testing.T) {
	st, dir := bigRunLog(t, "coreslot-protocol 2\n")
	if err := os.Mkdir(filepath.Join(dir, "queue"), 0o700); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(dir, "queue", "00000000000000000001-0000000001-000001")
	if err := os.WriteFile(dead, []byte("{}"), 0o600); err != nil { // no lock: dead
		t.Fatal(err)
	}
	notice := func(f string, args ...any) { t.Errorf("notice: "+f, args...) }
	if err := appendRunLog(st, store.RunRecord{Label: "x"}, notice); err != nil {
		t.Fatalf("appendRunLog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "log", "runs.1.jsonl")); err != nil {
		t.Errorf("the run log was not rotated: %v", err)
	}
	if _, err := os.Stat(dead); err == nil {
		t.Error("the dead ticket survived: the rotation section did not reconcile")
	}
	if g, err := st.ReadGen(); err != nil || g.ReconciledNS == 0 {
		t.Errorf("gen %+v (%v): reconciled_ns was not written", g, err)
	}
}

func TestTimeoutLogGate(t *testing.T) {
	cases := []struct {
		name          string
		holdPublished bool
		holdErr       error
		wantLog       bool
	}{
		{"hold published", true, nil, true},
		{"section busy, ticket published in format 1", false, context.DeadlineExceeded, true},
		{"format no longer understood", false, fmt.Errorf("%w: format 2", engine.ErrRefused), false},
		{"FORMAT now malformed", false, &store.StateError{Reason: "malformed FORMAT"}, false},
	}
	for _, c := range cases {
		if got := timeoutLogs(c.holdPublished, c.holdErr); got != c.wantLog {
			t.Errorf("%s: timeoutLogs = %v, want %v", c.name, got, c.wantLog)
		}
	}
}

func TestFailOpenNotice(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name          string
		opened, hold  bool
		err           error
		want, notWant string
	}{
		{"directory unusable", false, false, cause, "nothing was written", ""},
		{"no hold", true, false, cause, "the unbudgeted hold, the degraded record and the run log were not written: boom", ""},
		{"degraded failed", true, true, cause, "the degraded record was not written: boom", "hold"},
		{"everything written", true, true, nil, "", ""},
	}
	for _, c := range cases {
		got := failOpenNotice(c.opened, c.hold, c.err)
		if (c.want == "" && got != "") || !strings.Contains(got, c.want) || (c.notWant != "" && strings.Contains(got, c.notWant)) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// Under CORESLOT_STATE_DIR a Windows run reads <statedir>/config.toml (PROTOCOL.md section 5.3).
func TestWindowsConfigUnderOverride(t *testing.T) {
	dir := stateDir(t, "[platform]\nwindows_warning = false\n")
	t.Setenv("CORESLOT_STATE_DIR", dir)
	if got := windowsNotice(windowsConfig()); got != "" {
		t.Errorf("the state directory's configuration silences the warning, got %q", got)
	}
	t.Setenv("CORESLOT_STATE_DIR", stateDir(t, ""))
	if got := windowsNotice(windowsConfig()); got == "" {
		t.Error("without a configuration the warning is printed")
	}
}

// A waiting run limits its own Go runtime to one P while it waits and restores the setting
// afterwards; nothing is exported to the command's environment.
func TestWaitRunsOnOneP(t *testing.T) {
	before := runtime.GOMAXPROCS(0)
	if before == 1 {
		t.Skip("GOMAXPROCS is already 1 here")
	}
	var during int
	_, _ = waitOneP(func() (*engine.Grant, error) {
		during = runtime.GOMAXPROCS(0)
		return nil, nil
	})
	if during != 1 || runtime.GOMAXPROCS(0) != before {
		t.Fatalf("GOMAXPROCS %d while waiting and %d after, want 1 and %d", during, runtime.GOMAXPROCS(0), before)
	}
	env := []string{}
	for _, kv := range baseEnv(stateDir(t, "")) {
		if !strings.HasPrefix(kv, "GOMAXPROCS=") {
			env = append(env, kv)
		}
	}
	r := cli(t, env, "run", "--restart=yes", "--", "sh", "-c", "echo GOMAXPROCS=${GOMAXPROCS-unset}")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "GOMAXPROCS=unset" {
		t.Fatalf("exit %d, stdout %q (stderr %q): want GOMAXPROCS unset in the command", r.code, r.stdout, r.stderr)
	}
}
