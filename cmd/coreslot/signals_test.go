//go:build unix

package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// signalHelper runs the helpers of the signal tests; ok is false for an unknown name.
func signalHelper(name string, args []string) (code int, ok bool) {
	switch name {
	case "count-int":
		// Count SIGINTs: report readiness in args[0], wait for the first SIGINT, keep counting
		// for 1.5 s, write the count to args[1], then die of SIGINT.
		ch := make(chan os.Signal, 16)
		signal.Notify(ch, syscall.SIGINT)
		if err := os.WriteFile(args[0], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			return 1, true
		}
		<-ch
		n := 1
		quiet := time.After(1500 * time.Millisecond)
	count:
		for {
			select {
			case <-ch:
				n++
			case <-quiet:
				break count
			}
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(n)), 0o600); err != nil {
			return 1, true
		}
		signal.Reset(syscall.SIGINT)
		_ = unix.Kill(os.Getpid(), unix.SIGINT)
		time.Sleep(5 * time.Second)
		return 1, true
	case "ticker":
		// Write "pid ppid" to args[0], then append a byte to args[1] every 50 ms until args[2]
		// exists.
		if err := os.WriteFile(args[0], fmt.Appendf(nil, "%d %d", os.Getpid(), os.Getppid()), 0o600); err != nil {
			return 1, true
		}
		f, err := os.OpenFile(args[1], os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return 1, true
		}
		defer f.Close()
		deadline := time.Now().Add(helperDeadline)
		for {
			if _, err := os.Stat(args[2]); err == nil {
				return 0, true
			}
			if time.Now().After(deadline) {
				return 3, true
			}
			_, _ = f.Write([]byte("t"))
			time.Sleep(50 * time.Millisecond)
		}
	case "section-stop":
		// Inside inSection, send SIGTSTP to itself and write args[0]; after it, write args[1].
		err := inSection(func() error {
			_ = unix.Kill(os.Getpid(), unix.SIGTSTP)
			time.Sleep(300 * time.Millisecond)
			return os.WriteFile(args[0], nil, 0o600)
		})
		if err != nil {
			return 1, true
		}
		if err := os.WriteFile(args[1], nil, 0o600); err != nil {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// ---------------------------------------------------------------- PTY fixtures

// ptyProc is a process whose controlling terminal is a PTY; the test reads everything written
// to the terminal.
type ptyProc struct {
	cmd    *exec.Cmd
	master *os.File
	mu     sync.Mutex
	out    bytes.Buffer
}

// startPTY starts cmd as the leader of a new session whose controlling terminal is a new PTY,
// so its process group is the terminal's foreground group. Its group is killed when the test
// ends.
func startPTY(t *testing.T, cmd *exec.Cmd) *ptyProc {
	t.Helper()
	attrs := &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if cmd.Stdin != nil {
		attrs.Ctty = 1 // the terminal is the child's stdout, not its stdin
	}
	master, err := pty.StartWithAttrs(cmd, nil, attrs)
	if err != nil {
		t.Fatal(err)
	}
	p := &ptyProc{cmd: cmd, master: master}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			p.mu.Lock()
			p.out.Write(buf[:n])
			p.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		_ = cmd.Wait()
		master.Close()
	})
	return p
}

func (p *ptyProc) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

func (p *ptyProc) write(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(p.master, s); err != nil {
		t.Fatal(err)
	}
}

func (p *ptyProc) waitOutput(t *testing.T, want string, d time.Duration) {
	t.Helper()
	waitFor(t, d, fmt.Sprintf("terminal output %q", want), func() bool { return strings.Contains(p.output(), want) })
}

// wait waits for the process and returns its status.
func (p *ptyProc) wait(t *testing.T, d time.Duration) int {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("the process did not exit within %v (load: %s); terminal:\n%s", d, uptime(), p.output())
	}
	return exitStatus(p.cmd.ProcessState)
}

// shell starts an interactive shell with job control on a PTY.
func shell(t *testing.T, dir string) *ptyProc {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-i")
	cmd.Env = append(baseEnv(dir), "PS1=csprompt$ ", "ENV=", "BIN="+cliBinary(t), "HOME="+t.TempDir())
	p := startPTY(t, cmd)
	p.waitOutput(t, "csprompt$ ", 30*time.Second)
	return p
}

// readPID reads the first number of a file a helper wrote, waiting for the file.
func readPID(t *testing.T, path string) (pid, ppid int) {
	t.Helper()
	var b []byte
	waitFor(t, 30*time.Second, "the pid file "+filepath.Base(path), func() bool {
		var err error
		b, err = os.ReadFile(path)
		return err == nil && len(b) > 0
	})
	f := strings.Fields(string(b))
	pid, _ = strconv.Atoi(f[0])
	if len(f) > 1 {
		ppid, _ = strconv.Atoi(f[1])
	}
	return pid, ppid
}

// procState is the scheduler state of pid ("T" when stopped), "" once it is gone.
func procState(t *testing.T, pid int) string {
	t.Helper()
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return ""
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 || i+2 >= len(s) {
			return ""
		}
		st := s[i+2 : i+3]
		if st == "Z" || st == "X" {
			return ""
		}
		return strings.ToUpper(st)
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	st := strings.TrimSpace(string(out))
	if st == "" || st[0] == 'Z' {
		return ""
	}
	return st[:1]
}

// parentOf is pid's parent, 0 when it cannot be read.
func parentOf(t *testing.T, pid int) int {
	t.Helper()
	var field string
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return 0
		}
		s := string(b)
		if f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:]); len(f) > 1 {
			field = f[1]
		}
	} else if out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output(); err == nil {
		field = strings.TrimSpace(string(out))
	}
	n, _ := strconv.Atoi(field)
	return n
}

// killGroupOf kills pid's process group when the test ends, so that a job a shell started
// cannot outlive a failing test.
func killGroupOf(t *testing.T, pid int) {
	t.Helper()
	pg, err := unix.Getpgid(pid)
	if err != nil || pid <= 1 || pg <= 1 || pg == unix.Getpgrp() {
		return
	}
	t.Cleanup(func() {
		_ = unix.Kill(-pg, unix.SIGCONT)
		_ = unix.Kill(-pg, unix.SIGKILL)
	})
}

func gone(t *testing.T, pid int) func() bool {
	return func() bool { return procState(t, pid) == "" }
}

// tokensFree checks that no token of cpu 0..n-1 is locked.
func tokensFree(t *testing.T, dir string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if locked(t, dir, "cpu", i) {
			t.Errorf("token cpu/%d is still locked", i)
		}
	}
}

// ---------------------------------------------------------------- terminal mode

func TestTermAndHupToWrapperTerminalMode(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := stateDir(t, "[resources]\ncpu = 2\n")
			pidFile := filepath.Join(t.TempDir(), "pid")
			cmd := exec.Command(cliBinary(t), "run", "--need", "cpu=2", "--", "sh", "-c",
				`echo $$ > "$0"; exec sleep 60`, pidFile)
			cmd.Env = baseEnv(dir)
			p := startPTY(t, cmd)
			child, _ := readPID(t, pidFile)
			killGroupOf(t, child)
			if pg, _ := unix.Getpgid(child); pg != cmd.Process.Pid {
				t.Errorf("terminal mode: the command's group is %d, want coreslot's %d", pg, cmd.Process.Pid)
			}
			if err := unix.Kill(cmd.Process.Pid, sig); err != nil {
				t.Fatal(err)
			}
			if got := p.wait(t, 30*time.Second); got != 128+int(sig) {
				t.Errorf("coreslot exited %d, want %d; terminal:\n%s", got, 128+int(sig), p.output())
			}
			waitFor(t, 10*time.Second, "the command's exit", gone(t, child))
			tokensFree(t, dir, 2)
		})
	}
}

func TestCtrlCTerminalReachesChildOnce(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 2\n")
	tmp := t.TempDir()
	ready, count := filepath.Join(tmp, "ready"), filepath.Join(tmp, "count")
	cmd := exec.Command(cliBinary(t), append([]string{"run", "--need", "cpu=2", "--"}, helper(t, "count-int", ready, count)...)...)
	cmd.Env = baseEnv(dir)
	p := startPTY(t, cmd)
	child, _ := readPID(t, ready)
	killGroupOf(t, child)
	p.write(t, "\x03")
	if got := p.wait(t, 30*time.Second); got != 130 {
		t.Errorf("coreslot exited %d, want 130; terminal:\n%s", got, p.output())
	}
	b, err := os.ReadFile(count)
	if err != nil {
		t.Fatalf("the command did not report its SIGINTs: %v; terminal:\n%s", err, p.output())
	}
	if string(b) != "1" {
		t.Errorf("the command received %s SIGINTs, want exactly one", b)
	}
	waitFor(t, 10*time.Second, "the command's exit", gone(t, child))
	tokensFree(t, dir, 2)
}

func TestCtrlZAndFg(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 2\n")
	tmp := t.TempDir()
	pidFile, ticks, release := filepath.Join(tmp, "pid"), filepath.Join(tmp, "ticks"), filepath.Join(tmp, "release")
	sh := shell(t, dir)
	sh.write(t, fmt.Sprintf(`"$BIN" run --need cpu=2 -- env %s=ticker "$BIN" '%s' '%s' '%s'`+"\n", helperEnv, pidFile, ticks, release))
	child, cs := readPID(t, pidFile)
	killGroupOf(t, cs) // the job's group is not the shell's
	killGroupOf(t, child)
	sh.write(t, "\x1a")
	waitFor(t, 10*time.Second, "coreslot and the command stopping", func() bool {
		return procState(t, child) == "T" && procState(t, cs) == "T"
	})
	size := func() int64 {
		fi, err := os.Stat(ticks)
		if err != nil {
			return 0
		}
		return fi.Size()
	}
	before := size()
	time.Sleep(300 * time.Millisecond)
	if after := size(); after != before {
		t.Errorf("the command kept running while stopped: %d -> %d ticks", before, after)
	}
	sh.write(t, "fg\n")
	waitFor(t, 10*time.Second, "the command running again", func() bool { return size() > before+2 })
	if st := procState(t, cs); st == "T" {
		t.Errorf("coreslot is still stopped after fg")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "coreslot's exit", gone(t, cs))
	sh.write(t, "echo rc=$?\n")
	sh.waitOutput(t, "rc=0", 10*time.Second)
	tokensFree(t, dir, 2)
}

// ---------------------------------------------------------------- non-terminal mode

// Ctrl-Z on a foreground pipeline whose coreslot is in non-terminal mode: coreslot forwards
// SIGTSTP to the command's own group and stops too, so the shell sees the whole job stopped and
// prompts again; fg continues coreslot, which forwards SIGCONT to the command's group.
func TestPipelineCtrlZAndFg(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 2\n")
	tmp := t.TempDir()
	pidFile, ticks, release := filepath.Join(tmp, "pid"), filepath.Join(tmp, "ticks"), filepath.Join(tmp, "release")
	sh := shell(t, dir)
	sh.write(t, fmt.Sprintf(`true | "$BIN" run --need cpu=2 --label pipeline-z -- env %s=ticker "$BIN" '%s' '%s' '%s' | cat`+"\n",
		helperEnv, pidFile, ticks, release))
	child, cs := readPID(t, pidFile)
	killGroupOf(t, child)
	killGroupOf(t, cs) // the pipeline's group is not the shell's
	if pg, _ := unix.Getpgid(child); pg != child {
		t.Fatalf("non-terminal mode: the command's group is %d, want its own %d", pg, child)
	}
	size := func() int64 {
		fi, err := os.Stat(ticks)
		if err != nil {
			return 0
		}
		return fi.Size()
	}
	prompts := strings.Count(sh.output(), "csprompt$ ")
	sh.write(t, "\x1a")
	waitFor(t, 10*time.Second, "the shell prompting again (the job stopped)", func() bool {
		return strings.Count(sh.output(), "csprompt$ ") > prompts
	})
	if procState(t, child) != "T" || procState(t, cs) != "T" {
		t.Errorf("after Ctrl-Z: command %q, coreslot %q; want both stopped", procState(t, child), procState(t, cs))
	}
	before := size()
	time.Sleep(300 * time.Millisecond)
	if after := size(); after != before {
		t.Errorf("the command kept running while stopped: %d -> %d ticks", before, after)
	}
	sh.write(t, "fg\n")
	waitFor(t, 10*time.Second, "the command running again", func() bool { return size() > before+2 })
	if st := procState(t, cs); st == "T" {
		t.Errorf("coreslot is still stopped after fg")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "coreslot's exit", gone(t, cs))
	var exit any
	waitFor(t, 10*time.Second, "the run-log line", func() bool {
		for _, l := range runLog(t, dir) {
			if l["label"] == "pipeline-z" {
				exit = l["exit"]
				return true
			}
		}
		return false
	})
	if exit != float64(0) {
		t.Errorf("coreslot's status was %v, want 0", exit)
	}
	tokensFree(t, dir, 2)
}

func TestPipelineCtrlCTerminatesChildGroup(t *testing.T) {
	dir := stateDir(t, "[resources]\ncpu = 2\n")
	pidFile := filepath.Join(t.TempDir(), "pid")
	sh := shell(t, dir)
	// Stdin is a pipe, so coreslot is not in terminal mode although its group is the
	// terminal's foreground group: the command gets its own group, which Ctrl-C does not reach.
	sh.write(t, fmt.Sprintf(`true | "$BIN" run --need cpu=2 --label pipeline -- sh -c 'echo $$ > "$0"; exec sleep 60' '%s' | cat`+"\n", pidFile))
	child, _ := readPID(t, pidFile)
	killGroupOf(t, child)
	killGroupOf(t, parentOf(t, child)) // the pipeline's group is not the shell's
	if pg, _ := unix.Getpgid(child); pg != child {
		t.Errorf("non-terminal mode: the command's group is %d, want its own %d", pg, child)
	}
	sh.write(t, "\x03")
	waitFor(t, 30*time.Second, "the command's exit", gone(t, child))
	sh.write(t, "echo done-$((1+1))\n")
	sh.waitOutput(t, "done-2", 30*time.Second)
	var exit any
	waitFor(t, 10*time.Second, "the run-log line", func() bool {
		for _, l := range runLog(t, dir) {
			if l["label"] == "pipeline" {
				exit = l["exit"]
				return true
			}
		}
		return false
	})
	if exit != float64(130) {
		t.Errorf("coreslot's status was %v, want 130", exit)
	}
	tokensFree(t, dir, 2)
}

func TestTermAndHupNonTerminal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) { signalNonTerminal(t, sig) })
	}
}

func TestIntQuitToNonTerminalWrapperPID(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGQUIT} {
		t.Run(sig.String(), func(t *testing.T) { signalNonTerminal(t, sig) })
	}
}

// signalNonTerminal sends sig to the pid of a coreslot whose stdin is not a terminal: it must
// reach the command's own group, and coreslot exits with the command's status.
func signalNonTerminal(t *testing.T, sig syscall.Signal) {
	dir := stateDir(t, "[resources]\ncpu = 2\n")
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := cliCmd(t, baseEnv(dir), "run", "--need", "cpu=2", "--", "sh", "-c",
		`ulimit -c 0; echo $$ > "$0"; exec sleep 60`, pidFile)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child, _ := readPID(t, pidFile)
	killGroupOf(t, child)
	if pg, _ := unix.Getpgid(child); pg != child {
		t.Errorf("the command's group is %d, want its own %d", pg, child)
	}
	if err := unix.Kill(cmd.Process.Pid, sig); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("coreslot did not exit within 30 s of %v (load: %s); stderr %q", sig, uptime(), errb.String())
	}
	if got := exitStatus(cmd.ProcessState); got != 128+int(sig) {
		t.Errorf("coreslot exited %d, want %d (stderr %q)", got, 128+int(sig), errb.String())
	}
	waitFor(t, 10*time.Second, "the command's exit", gone(t, child))
	tokensFree(t, dir, 2)
}

// ---------------------------------------------------------------- the section

func TestInSectionDefersStop(t *testing.T) {
	tmp := t.TempDir()
	inside, after := filepath.Join(tmp, "inside"), filepath.Join(tmp, "after")
	cmd := exec.Command(cliBinary(t), inside, after)
	cmd.Env = append(baseEnv(tmp), helperEnv+"=section-stop")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL); _ = cmd.Wait() })
	exists := func(p string) func() bool {
		return func() bool { _, err := os.Stat(p); return err == nil }
	}
	waitFor(t, 10*time.Second, "the section's end", exists(inside))
	waitFor(t, 10*time.Second, "the deferred stop", func() bool { return procState(t, cmd.Process.Pid) == "T" })
	if _, err := os.Stat(after); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the process went on after the section instead of stopping")
	}
	if err := unix.Kill(cmd.Process.Pid, unix.SIGCONT); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the run after continuing", exists(after))
}

// sectionMethods are the engine and store calls that may enter the admission section. The run
// must make each of them inside inSection, so that it never stops itself while holding the
// section: its self-stop is deferred only there.
var sectionMethods = map[string]bool{"Enqueue": true, "EnqueueContext": true, "Step": true,
	"Poll": true, "Withdraw": true, "FailOpen": true, "UnbudgetedHold": true, "Snapshot": true,
	"Enter": true, "TryEnter": true, "Adopt": true, "Cancel": true}

// TestSectionCallsAreInSection checks the CLI's sources: every call (or method value) of a
// method in sectionMethods, and every Close of a grant (named g), sits inside an argument of
// inSection.
func TestSectionCallsAreInSection(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			var stack []ast.Node
			ast.Inspect(f, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				x, isIdent := sel.X.(*ast.Ident)
				grantClose := sel.Sel.Name == "Close" && isIdent && x.Name == "g"
				if !sectionMethods[sel.Sel.Name] && !grantClose {
					return true
				}
				if isIdent && (x.Name == "signal" || x.Name == "unix") {
					return true
				}
				checked++
				for i := len(stack) - 2; i >= 0; i-- {
					if call, ok := stack[i].(*ast.CallExpr); ok {
						if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "inSection" && stack[i+1] != call.Fun {
							return true
						}
					}
				}
				t.Errorf("%s: %s.%s is called outside inSection", fset.Position(sel.Pos()), exprString(sel.X), sel.Sel.Name)
				return true
			})
		}
	}
	if checked < 8 {
		t.Fatalf("only %d section calls found: the check no longer sees the run's code", checked)
	}
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.CompositeLit:
		return "composite"
	}
	return "expr"
}
