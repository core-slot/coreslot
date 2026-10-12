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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot"
	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/spec"
	"github.com/Nezhinskiy/coreslot/internal/store"
)

// The test binary doubles as the coreslot binary and as the helper commands the tests run:
// CORESLOT_CLI_TEST_HELPER selects a helper, and otherwise CORESLOT_CLI_TEST_AS_MAIN makes it
// coreslot itself, so nested runs started by a command reach the same binary.
const (
	helperEnv = "CORESLOT_CLI_TEST_HELPER"
	asMainEnv = "CORESLOT_CLI_TEST_AS_MAIN"
)

func TestMain(m *testing.M) {
	if h := os.Getenv(helperEnv); h != "" {
		os.Exit(runHelper(h, os.Args[1:]))
	}
	if os.Getenv(asMainEnv) != "" {
		main()
	}
	os.Exit(m.Run())
}

// helperDeadline ends a waiting helper whose test failed before releasing it, so that it can
// never loop on after the test.
const helperDeadline = 2 * time.Minute

// runHelper runs one helper command of the tests.
func runHelper(name string, args []string) int {
	switch name {
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
		e, _, err := openEngine()
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
			fmt.Printf("hold %s unbudgeted=%v\n", h.Label, h.Unbudgeted)
		}
		return 0
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

// baseEnv is the test's environment without any coreslot context, plus the state directory
// and the switch that makes the test binary coreslot.
func baseEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CORESLOT_") {
			env = append(env, kv)
		}
	}
	return append(env, "CORESLOT_STATE_DIR="+dir, asMainEnv+"=1")
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

// shCmd is `/bin/sh -c script args...` in its own process group, killed with its group when the
// test ends.
func shCmd(t *testing.T, env []string, script string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"-c", script}, args...)...)
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
func locked(t *testing.T, dir, res string, i int) bool {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "res", res, strconv.Itoa(i)))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	return false
}

// holdToken locks res/<res>/<i> from the test process until the test ends.
func holdToken(t *testing.T, dir, res string, i int) {
	t.Helper()
	d := filepath.Join(dir, "res", res)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(d, strconv.Itoa(i)), os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
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
		{"library state error", &coreslot.StateError{Reason: "symlink"}, 0, true},
		{"cli state error", &stateError{Reason: "unusable"}, 0, true},
		{"wrapped state error", fmt.Errorf("open: %w", &coreslot.StateError{Reason: "x"}), 0, true},
		{"strict turns a state error into a refusal", strictRefusal(&coreslot.StateError{Reason: "x"}, 75), 75, false},
		{"strict with --refusal-code", strictRefusal(&stateError{Reason: "x"}, 99), 99, false},
		{"interrupted", &interruptError{Sig: syscall.SIGINT}, 130, false},
		{"not found", &exec.Error{Name: "nope", Err: exec.ErrNotFound}, 127, false},
		{"missing path", &os.PathError{Op: "fork/exec", Path: "/nope", Err: syscall.ENOENT}, 127, false},
		{"not executable", &os.PathError{Op: "fork/exec", Path: "/etc", Err: syscall.EACCES}, 126, false},
		{"other", errors.New("boom"), 70, false},
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
		{"run", "--need", "cpu=4..2", "--", "true"},
		{"run", "--need", "cpu=1", "--need", "cpu=2", "--", "true"},
		{"run", "--need", "cpu=1", "--", "echo", "{n:gpu}"},
		{"run", "--subst", "--", "make", "-j{n:gpu}"},
		{"run", "--bogus", "--", "true"},
		{"run", "--need", "gpu=1", "--", "echo", "{n}"},
		{"run"},
		{"run", "--on-timeout", "maybe", "--", "true"},
	} {
		if r := cli(t, baseEnv(dir), args...); r.code != 64 {
			t.Errorf("%q: exit %d, want 64 (stderr %q)", args, r.code, r.stderr)
		}
	}
	if q := entries(t, dir, "queue"); len(q) != 0 {
		t.Errorf("a usage error queued: %q", q)
	}
}

// ---------------------------------------------------------------- runs

func TestRunPassesExitStatus(t *testing.T) {
	dir := stateDir(t, "")
	if r := cli(t, baseEnv(dir), "run", "--", "sh", "-c", "exit 3"); r.code != 3 {
		t.Errorf("exit 3: got %d (stderr %q)", r.code, r.stderr)
	}
	if r := cli(t, baseEnv(dir), "run", "--", "sh", "-c", "kill -KILL $$"); r.code != 137 {
		t.Errorf("SIGKILL: got %d, want 137 (stderr %q)", r.code, r.stderr)
	}
}

func TestRunMissingCommandFreesEverything(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	for _, cmd := range []string{"coreslot-no-such-command", filepath.Join(dir, "missing")} {
		r := cli(t, baseEnv(dir), "run", "--need", "cpu=2..4", "--", cmd)
		if r.code != 127 {
			t.Errorf("%s: exit %d, want 127 (stderr %q)", cmd, r.code, r.stderr)
		}
		if h := entries(t, dir, "holds"); len(h) != 0 {
			t.Errorf("%s: holds left: %q", cmd, h)
		}
		for i := 0; i < 4; i++ {
			if locked(t, dir, "cpu", i) {
				t.Errorf("%s: token %d still locked", cmd, i)
			}
		}
	}
	notExec := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := cli(t, baseEnv(dir), "run", "--", notExec); r.code != 126 {
		t.Errorf("not executable: exit %d, want 126 (stderr %q)", r.code, r.stderr)
	}
}

func TestCtrlCWhileWaitingExits130(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 1\n")
	if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	holdToken(t, dir, "cpu", 0)
	cmd := cliCmd(t, baseEnv(dir), "run", "--", "true")
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
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	r := cli(t, baseEnv(dir), "run", "--no-wait", "--need", "cpu=2..3", "--", "echo", "{n}")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "3" {
		t.Errorf("exit %d stdout %q stderr %q, want 0 and a grant of 3", r.code, r.stdout, r.stderr)
	}
	tokensFree(t, dir, 4)
}

func TestNoWaitRefusesWithin2s(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 2\n")
	if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
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
		r := cli(t, baseEnv(dir), "run", "--no-wait", "--", "true")
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("refused after %v, want within 2 s (load: %s)", took, uptime())
		}
		if r.code != 75 || !strings.Contains(r.stderr, "--no-wait") {
			t.Errorf("exit %d stderr %q, want 75 and a --no-wait diagnostic", r.code, r.stderr)
		}
	})

	t.Run("budget full", func(t *testing.T) {
		holdToken(t, dir, "cpu", 0)
		holdToken(t, dir, "cpu", 1)
		r := cli(t, baseEnv(dir), "run", "--no-wait", "--refusal-code", "9", "--", "true")
		if r.code != 9 {
			t.Errorf("exit %d stderr %q, want the refusal code 9", r.code, r.stderr)
		}
	})
}

func TestTimeoutRunRecordsUnbudgeted(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 1\n")
	if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
		t.Fatalf("setup run: %d %q", r.code, r.stderr)
	}
	holdToken(t, dir, "cpu", 0)
	before := len(runLog(t, dir))

	r := cli(t, baseEnv(dir), "run", "--need", "cpu=1", "--timeout", "1s", "--", "true")
	if r.code != 75 {
		t.Fatalf("--on-timeout defaults to fail: exit %d, want 75 (stderr %q)", r.code, r.stderr)
	}

	r = cli(t, append(baseEnv(dir), "SNAPSHOT="+strings.Join(helper(t, "snapshot"), " ")), "run", "--need", "cpu=1",
		"--label", "late", "--timeout", "1s", "--on-timeout", "run", "--", "sh", "-c",
		`echo "granted=$CORESLOT_GRANTED_CPU unbudgeted=$CORESLOT_UNBUDGETED tokens=$CORESLOT_TOKEN_FDS"; $SNAPSHOT`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if got := strings.Split(strings.TrimSpace(r.stdout), "\n")[0]; got != "granted=1 unbudgeted=timeout tokens=" {
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
	if fmt.Sprint(last["unbudgeted_resources"]) != "[cpu]" || last["fail_open"] != "timeout" {
		t.Errorf("run log line %v: want unbudgeted_resources [cpu] and the reason timeout", last)
	}
	if h := entries(t, dir, "holds"); len(h) != 0 {
		t.Errorf("the unbudgeted hold outlived the run: %q", h)
	}
	if _, err := os.Stat(filepath.Join(dir, "degraded")); err == nil {
		t.Error("a timeout recorded degraded mode")
	}
}

func TestFailOpenExportsUnbudgeted(t *testing.T) {
	show := `echo "granted=$CORESLOT_GRANTED_CPU unbudgeted=$CORESLOT_UNBUDGETED"`

	t.Run("unusable directory", func(t *testing.T) {
		dir := stateDir(t, "")
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		r := cli(t, baseEnv(dir), "run", "--need", "cpu=3..8", "--", "sh", "-c", show)
		if r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		out := strings.TrimSpace(r.stdout)
		if !strings.HasPrefix(out, "granted=3 unbudgeted=") || out == "granted=3 unbudgeted=" {
			t.Errorf("command saw %q, want granted=3 and a reason", out)
		}
		if !strings.Contains(r.stderr, "state error") || !strings.Contains(r.stderr, "nothing was written") {
			t.Errorf("stderr %q: want the diagnostic and that nothing was recorded", r.stderr)
		}
		if names := entries(t, dir, ""); len(names) != 0 {
			t.Errorf("wrote into an unusable directory: %q", names)
		}
	})

	t.Run("invalid configuration", func(t *testing.T) {
		dir := stateDir(t, "[resources]\ncpu = \"bogus\"\n")
		r := cli(t, baseEnv(dir), "run", "--need", "cpu=2", "--", "sh", "-c",
			show+`; ls "$CORESLOT_STATE_DIR/holds"`)
		if r.code != 0 {
			t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
		}
		if !strings.Contains(r.stdout, "granted=2 unbudgeted=configuration") {
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
		dir := stateDir(t, "[resources]\ncpu = \"bogus\"\n")
		marker := filepath.Join(t.TempDir(), "ran")
		r := cli(t, baseEnv(dir), "run", "--strict", "--", "touch", marker)
		if r.code != 75 {
			t.Errorf("exit %d, want 75 (stderr %q)", r.code, r.stderr)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Error("the command ran under --strict")
		}
	})
}

func TestRunAppendsOneLogLine(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 4\n")
	env := append(baseEnv(dir), "WAVE_E_SECRET=env-secret-value")
	r := cli(t, env, "run", "--need", "cpu=2..3", "--label", "gate:unit", "--", "sh", "-c", "exit 0", "argv-secret-value")
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
	if line["label"] != "gate:unit" || fmt.Sprint(line["need"]) != "map[cpu:[2 3]]" ||
		fmt.Sprint(line["grant"]) != "map[cpu:3]" || line["exit"] != float64(0) {
		t.Errorf("run log line %v", line)
	}
	if _, nested := line["nested"]; nested {
		t.Errorf("an ordinary run's line is marked nested: %v", line)
	}
}

func TestClampNotice(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 10\n")
	r := cli(t, baseEnv(dir), "run", "--need", "cpu=16..16", "--", "echo", "{n}")
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if got := strings.TrimSpace(r.stdout); got != "10" {
		t.Errorf("{n} = %q, want 10", got)
	}
	want := "coreslot: cpu: requested 16, budget is 10; running alone with 10\n"
	if !strings.Contains(r.stderr, want) {
		t.Errorf("stderr %q, want the notice %q", r.stderr, want)
	}
}

func TestChildRlimitRaised(t *testing.T) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	if uint64(lim.Max) < 256+256 {
		t.Skipf("the hard limit %d does not allow 256 + 256", lim.Max)
	}
	dir := stateDir(t, "")
	cmd := shCmd(t, baseEnv(dir), `ulimit -S -n 256 && exec "$0" run -- sh -c 'ulimit -n'`, cliBinary(t))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if n, err := strconv.Atoi(got); err == nil && n < 256+256 {
		t.Errorf("the command's soft limit is %d, want at least 512", n)
	} else if err != nil && got != "unlimited" {
		t.Errorf("ulimit -n printed %q", got)
	}
}

func TestCLIKeepsCopiesUntilChildExits(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 2\n")
	tmp := t.TempDir()
	closed, release := filepath.Join(tmp, "closed"), filepath.Join(tmp, "release")
	args := append([]string{"run", "--need", "cpu=2", "--"}, helper(t, "close-and-wait", closed, release)...)
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
	for i := 0; i < 2; i++ {
		if !locked(t, dir, "cpu", i) {
			t.Errorf("token %d was released while the command still runs", i)
		}
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if got := exitStatus(cmd.ProcessState); got != 0 {
		t.Fatalf("exit %d (stderr %q)", got, errb.String())
	}
	for i := 0; i < 2; i++ {
		if locked(t, dir, "cpu", i) {
			t.Errorf("token %d is still locked after the command exited", i)
		}
	}
}

// ---------------------------------------------------------------- the waiting line

type fakeStepper struct{ calls, holderCalls int }

// due is a non-head waiter's: its next election is a second away.
func (s *fakeStepper) due() time.Duration { return time.Second }

func (s *fakeStepper) step(context.Context) (engine.StepResult, error) {
	s.calls++
	return engine.Waiting{Position: 2}, nil
}

func (s *fakeStepper) holders() ([]engine.HolderInfo, error) {
	s.holderCalls++
	return []engine.HolderInfo{{Label: "gate", PID: 4711, Grant: map[string]int{"cpu": 12},
		StartNS: time.Now().Add(-192 * time.Second).UnixNano()}}, nil
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

func TestWaitingLineEvery30s(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := &fakeClock{limit: 95 * time.Second, cancel: cancel}
	s := &fakeStepper{}
	var out bytes.Buffer
	r := &run{o: runOptions{need: map[string]spec.Range{"cpu": {Min: 4, Max: 12}}}}
	_, err := waitLoop(ctx, s, clk, &out, r.waitingLine)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitLoop = %v, want the context's end", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d waiting lines in 95 s, want 3 (at 30, 60 and 90 s):\n%s", len(lines), out.String())
	}
	for i, want := range []string{"(30s)", "(1m00s)", "(1m30s)"} {
		if !strings.HasPrefix(lines[i], "coreslot: waiting for cpu 4..12: position 2 "+want+"; holders: gate pid 4711 cpu12 (3m1") {
			t.Errorf("line %d = %q", i, lines[i])
		}
	}
	if s.holderCalls != 3 {
		t.Errorf("holders listed %d times in 95 s, want 3: only for the waiting lines", s.holderCalls)
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
	if r := cli(t, baseEnv(dir), "run", "--", "true"); r.code != 0 {
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
	r := cli(t, baseEnv(dir), "run", "--timeout", "1s", "--", "true")
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

// enqueue is bounded by its context and leaves no goroutine behind.
func TestEnqueueBoundedLeavesNothing(t *testing.T) {
	dir := stateDir(t, "")
	t.Setenv("CORESLOT_STATE_DIR", dir)
	e, _, err := openEngine()
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
	tk, err := enqueue(ctx, e, spec.Request{Need: map[string]spec.Range{"cpu": {Min: 1, Max: 1}}})
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
	for _, format := range []string{"coreslot-protocol 2\n", "coreslot-protocol x\n"} {
		st, dir := bigRunLog(t, format)
		var notices []string
		notice := func(f string, args ...any) { notices = append(notices, fmt.Sprintf(f, args...)) }
		if err := appendRunLog(st, store.RunRecord{Label: "x"}, notice); err == nil {
			t.Errorf("FORMAT %q: appendRunLog: want an error", format)
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
		"unknown":   {"coreslot-protocol 2\n", "FORMAT is missing or names a format this client does not implement"},
		"malformed": {"coreslot-protocol x\n", "malformed FORMAT"},
	} {
		format := c.format
		t.Run(name, func(t *testing.T) {
			dir := stateDir(t, "")
			env := append(baseEnv(dir), "S="+dir, "F="+format)
			r := cli(t, env, "run", "--label", "changed", "--", "sh", "-c", `printf %s "$F" > "$S/FORMAT"`)
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
// which comes before the append, writes the missing FORMAT again (format 1), and the re-read then
// understands it.
func TestRunLogAfterTheCommandRemovesFormat(t *testing.T) {
	dir := stateDir(t, "")
	env := append(baseEnv(dir), "S="+dir)
	r := cli(t, env, "run", "--label", "removed", "--", "sh", "-c", `rm "$S/FORMAT"`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "FORMAT")); err != nil || string(b) != "coreslot-protocol 1\n" {
		t.Errorf("FORMAT after the run: %q, %v; want format 1 written again", b, err)
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
	st, dir := bigRunLog(t, "coreslot-protocol 1\n")
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
	st, dir := bigRunLog(t, "coreslot-protocol 1\n")
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
		name                      string
		holdPublished             bool
		holdErr                   error
		ticketUnderstood, wantLog bool
	}{
		{"hold published", true, nil, true, true},
		{"section busy, ticket published in format 1", false, context.DeadlineExceeded, true, true},
		{"format no longer understood", false, fmt.Errorf("%w: format 2", engine.ErrRefused), true, false},
		{"FORMAT now malformed", false, &store.StateError{Reason: "malformed FORMAT"}, true, false},
		{"token-only ticket", false, context.DeadlineExceeded, false, false},
	}
	for _, c := range cases {
		if got := timeoutLogs(c.holdPublished, c.holdErr, c.ticketUnderstood); got != c.wantLog {
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

// Under CORESLOT_STATE_DIR a Windows run reads <statedir>/config.toml (R-C1).
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
	r := cli(t, env, "run", "--", "sh", "-c", "echo GOMAXPROCS=${GOMAXPROCS-unset}")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "GOMAXPROCS=unset" {
		t.Fatalf("exit %d, stdout %q (stderr %q): want GOMAXPROCS unset in the command", r.code, r.stdout, r.stderr)
	}
}
