//go:build unix

// Command floor measures what a waiting process costs before coreslot's own work: the CPU of
// a Go process that only wakes once per second, of one that also makes, once per second, the
// system calls a waiter that is not the head makes (without the engine), and of a real library
// waiter that is not the head, in a queue whose gen does not change. Each mode runs -procs
// processes at once for -duration after -warmup and reports their CPU (getrusage RUSAGE_SELF,
// user + system) per process-second.
//
//	floor -mode sleep    -procs 20 -duration 60s
//	floor -mode syscalls -procs 20 -duration 60s   # also: refresh, cancel, gen, probe alone
//	floor -mode waiter   -procs 20 -duration 60s [-cpuprofile DIR]
//	floor -mode cli      -procs 20 -duration 60s -coreslot BIN
//
// The cli mode runs `coreslot run --need cpu=1 -- true` waiters, which cannot report their own
// CPU: it runs two batches, one for the warm-up alone and one for the warm-up plus the window,
// ends each waiter with SIGINT (it withdraws and exits), and reports the difference of the
// batches' mean wait4 rusage over the window.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot"
)

// The once-per-second work of a non-head waiter, as system calls (internal/engine, Step):
// refresh (futimes or utimensat), the cancel check (open: ENOENT), the gen read (open, fstat,
// fcntl F_GETFL and F_SETFL, read, read at EOF, close), and the election's examination of the
// nearest ticket ahead (lstat; then open, fstat, the two fcntls, flock LOCK_NB, close).
var families = map[string][]string{
	"sleep":    nil,
	"refresh":  {"refresh"},
	"cancel":   {"cancel"},
	"gen":      {"gen"},
	"probe":    {"probe"},
	"syscalls": {"refresh", "cancel", "gen", "probe"},
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		os.Exit(child(os.Args[2:]))
	}
	os.Exit(parent(os.Args[1:]))
}

func parent(args []string) int {
	fs := flag.NewFlagSet("floor", flag.ContinueOnError)
	mode := fs.String("mode", "sleep", "sleep, refresh, cancel, gen, probe, syscalls or waiter")
	procs := fs.Int("procs", 20, "processes run at once")
	duration := fs.Duration("duration", 60*time.Second, "measured window")
	warmup := fs.Duration("warmup", 10*time.Second, "wait before the window")
	profDir := fs.String("cpuprofile", "", "waiter mode: write each child's CPU profile of the window here")
	cliBin := fs.String("coreslot", "", "cli mode: the coreslot binary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if _, ok := families[*mode]; !ok && *mode != "waiter" && *mode != "cli" {
		fmt.Fprintln(os.Stderr, "floor: unknown mode", *mode)
		return 2
	}
	dir, err := os.MkdirTemp("", "coreslot-floor-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "floor:", err)
		return 2
	}
	defer os.RemoveAll(dir)
	stop, err := prepare(*mode, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "floor:", err)
		return 2
	}
	defer stop()

	if *mode == "cli" {
		return cliMode(*cliBin, dir, *procs, *warmup, *duration)
	}
	self, _ := os.Executable()
	var cmds []*exec.Cmd
	var outs []*bufio.Scanner
	for i := 0; i < *procs; i++ {
		prof := ""
		if *profDir != "" {
			prof = filepath.Join(*profDir, fmt.Sprintf("%s-%d.pprof", *mode, i))
		}
		cmd := exec.Command(self, "child", *mode, dir, warmup.String(), duration.String(), prof)
		cmd.Stderr = os.Stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			fmt.Fprintln(os.Stderr, "floor:", err)
			return 2
		}
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "floor:", err)
			return 2
		}
		cmds, outs = append(cmds, cmd), append(outs, bufio.NewScanner(out))
	}
	var per []float64 // us per process-second
	for i, cmd := range cmds {
		var cpu, wall float64
		for outs[i].Scan() {
			if f := strings.Fields(outs[i].Text()); len(f) == 3 && f[0] == "CPU" {
				cpu, _ = strconv.ParseFloat(f[1], 64)
				wall, _ = strconv.ParseFloat(f[2], 64)
			}
		}
		if err := cmd.Wait(); err != nil || wall == 0 {
			fmt.Fprintf(os.Stderr, "floor: child %d: %v (no result)\n", i, err)
			return 2
		}
		per = append(per, cpu/1e3/(wall/1e9))
	}
	slices.Sort(per)
	sum := 0.0
	for _, v := range per {
		sum += v
	}
	fmt.Printf("mode %s: %d processes, %s window (GOOS %s, %s): us CPU per process-second: min %.1f, median %.1f, mean %.1f, max %.1f\n",
		*mode, len(per), *duration, runtime.GOOS, runtime.Version(), per[0], per[len(per)/2], sum/float64(len(per)), per[len(per)-1])
	return 0
}

// prepare builds what the children use. Syscall modes: a gen file, an own ticket file per
// child (created by the child), and a ticket "ahead" whose lock this process holds, so a probe
// finds it live. Waiter mode: a state directory with cpu = 1, the token adopted by this process
// and a ticket ahead of every child kept fresh by this process's Polls.
func prepare(mode, dir string) (stop func(), err error) {
	if mode != "waiter" && mode != "cli" {
		if err := os.WriteFile(filepath.Join(dir, "gen"), []byte("{\"gen\":12345,\"reconciled_ns\":1760000000000000000}\n"), 0o600); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(filepath.Join(dir, "ahead"), os.O_RDONLY|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return nil, err
		}
		return func() { f.Close() }, nil
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[resources]\ncpu = 1\n"), 0o600); err != nil {
		return nil, err
	}
	c, err := coreslot.Open(coreslot.Options{StateDir: dir})
	if err != nil {
		return nil, err
	}
	need := map[string]coreslot.Range{"cpu": {Min: 1, Max: 1}}
	g, _, err := c.Adopt(coreslot.Request{Label: "floor-holder", Need: need})
	if err != nil {
		return nil, err
	}
	head, err := c.Enqueue(coreslot.Request{Label: "floor-head", Need: need})
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		for {
			_, _, _ = head.Poll() // the head: refreshed, never admitted
			select {
			case <-done:
				return
			case <-time.After(time.Second):
			}
		}
	}()
	return func() { close(done); _ = head.Cancel(); g.Close() }, nil
}

func child(args []string) int {
	mode, dir := args[0], args[1]
	warmup, _ := time.ParseDuration(args[2])
	duration, _ := time.ParseDuration(args[3])
	prof := args[4]
	if mode == "waiter" {
		c, err := coreslot.Open(coreslot.Options{StateDir: dir})
		if err != nil {
			fmt.Fprintln(os.Stderr, "floor child:", err)
			return 1
		}
		w, err := c.Enqueue(coreslot.Request{Need: map[string]coreslot.Range{"cpu": {Min: 1, Max: 1}}})
		if err != nil {
			fmt.Fprintln(os.Stderr, "floor child:", err)
			return 1
		}
		go func() { _, _ = w.Wait(context.Background()) }()
	} else {
		work, err := syscallWork(families[mode], dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "floor child:", err)
			return 1
		}
		go func() { // the shape of Wait's loop: a timer reset once per second
			t := time.NewTimer(0)
			for range t.C {
				work()
				t.Reset(time.Second)
			}
		}()
	}
	time.Sleep(warmup)
	if prof != "" {
		f, err := os.Create(prof)
		if err == nil {
			runtime.SetCPUProfileRate(1000)
			_ = pprof.StartCPUProfile(f)
			defer func() { pprof.StopCPUProfile(); f.Close() }()
		}
	}
	c0, w0 := selfCPU(), time.Now()
	time.Sleep(duration)
	c1, w1 := selfCPU(), time.Now()
	fmt.Printf("CPU %d %d\n", c1-c0, w1.Sub(w0).Nanoseconds())
	return 0
}

// syscallWork returns the listed families of a non-head waiter's per-second system calls.
func syscallWork(fams []string, dir string) (func(), error) {
	own, err := os.CreateTemp(dir, "ticket-")
	if err != nil {
		return nil, err
	}
	gen, ahead := filepath.Join(dir, "gen"), filepath.Join(dir, "ahead")
	cancel := filepath.Join(dir, "cancel-"+strconv.Itoa(os.Getpid()))
	buf := make([]byte, 512)
	open := func(p string) (int, error) { // as store.OpenFile
		fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			return -1, err
		}
		var st unix.Stat_t
		_ = unix.Fstat(fd, &st)
		_ = unix.SetNonblock(fd, false)
		return fd, nil
	}
	steps := map[string]func(){
		"refresh": func() { touch(int(own.Fd()), time.Now().UnixNano()) },
		"cancel":  func() { _, _ = open(cancel) },
		"gen": func() {
			if fd, err := open(gen); err == nil {
				for n, _ := unix.Read(fd, buf); n > 0; n, _ = unix.Read(fd, buf) {
				}
				unix.Close(fd)
			}
		},
		"probe": func() {
			var st unix.Stat_t
			_ = unix.Lstat(ahead, &st)
			if fd, err := open(ahead); err == nil {
				_ = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
				unix.Close(fd)
			}
		},
	}
	var fns []func()
	for _, f := range fams {
		fns = append(fns, steps[f])
	}
	return func() {
		for _, f := range fns {
			f()
		}
	}, nil
}

// cliMode measures CLI waiters (see the package comment).
func cliMode(bin, dir string, procs int, warmup, duration time.Duration) int {
	batch := func(life time.Duration) (float64, error) {
		var cmds []*exec.Cmd
		for i := 0; i < procs; i++ {
			cmd := exec.Command(bin, "run", "--need", "cpu=1", "--", "true")
			cmd.Env = append(os.Environ(), "CORESLOT_STATE_DIR="+dir)
			if err := cmd.Start(); err != nil {
				return 0, err
			}
			cmds = append(cmds, cmd)
		}
		time.Sleep(life)
		total := 0.0
		for _, cmd := range cmds {
			_ = cmd.Process.Signal(syscall.SIGINT)
			_ = cmd.Wait() // exits 130 after withdrawing
			ru := cmd.ProcessState.SysUsage().(*syscall.Rusage)
			total += float64(ru.Utime.Nano() + ru.Stime.Nano())
		}
		return total / float64(procs), nil
	}
	short, err := batch(warmup)
	if err != nil {
		fmt.Fprintln(os.Stderr, "floor:", err)
		return 2
	}
	long, err := batch(warmup + duration)
	if err != nil {
		fmt.Fprintln(os.Stderr, "floor:", err)
		return 2
	}
	fmt.Printf("mode cli: %d processes per batch, %s window (GOOS %s, %s): us CPU per process-second: %.1f (mean of the long batch %.0f us minus the short batch %.0f us, over the window)\n",
		procs, duration, runtime.GOOS, runtime.Version(), (long-short)/1e3/duration.Seconds(), long/1e3, short/1e3)
	return 0
}

func selfCPU() int64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return ru.Utime.Nano() + ru.Stime.Nano()
}
