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
	case "close-and-wait":
		// Close every descriptor from 3, report it in args[0], keep working until args[1] exists.
		for fd := 3; fd < 1024; fd++ {
			unix.Close(fd)
		}
		if err := os.WriteFile(args[0], nil, 0o600); err != nil {
			return 1
		}
		for {
			if _, err := os.Stat(args[1]); err == nil {
				return 0
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
		{"strict turns a state error into a refusal", &refusalError{Msg: "state error; --strict"}, 75, false},
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

	r = cli(t, baseEnv(dir), "run", "--need", "cpu=1", "--timeout", "1s", "--on-timeout", "run", "--",
		"sh", "-c", `echo "granted=$CORESLOT_GRANTED_CPU unbudgeted=$CORESLOT_UNBUDGETED tokens=$CORESLOT_TOKEN_FDS"`)
	if r.code != 0 {
		t.Fatalf("exit %d (stderr %q)", r.code, r.stderr)
	}
	if got := strings.TrimSpace(r.stdout); got != "granted=1 unbudgeted= tokens=" {
		t.Errorf("command saw %q", got)
	}
	lines := runLog(t, dir)
	if len(lines) != before+1 {
		t.Fatalf("run log has %d lines, want %d", len(lines), before+1)
	}
	last := lines[len(lines)-1]
	if fmt.Sprint(last["unbudgeted_resources"]) != "[cpu]" || last["fail_open"] != "" {
		t.Errorf("run log line %v: want unbudgeted_resources [cpu] and no fail-open reason", last)
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
		if !strings.Contains(r.stderr, "state error") || !strings.Contains(r.stderr, "not written") {
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

type fakeStepper struct{ calls int }

func (s *fakeStepper) Step(context.Context) (engine.StepResult, error) {
	s.calls++
	return engine.Waiting{Position: 2, Holders: []engine.HolderInfo{{Label: "gate", PID: 4711,
		Grant: map[string]int{"cpu": 12}, StartNS: time.Now().Add(-192 * time.Second).UnixNano()}}}, nil
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
	if s.calls < 95*4-1 {
		t.Errorf("%d steps in 95 s, want one every 250 ms", s.calls)
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)
