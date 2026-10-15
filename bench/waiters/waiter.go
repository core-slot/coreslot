//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/core-slot/coreslot"
)

// Waiter exit codes, read by the harness.
const (
	exitAdmitted  = 0
	exitError     = 1
	exitWithdrawn = 3 // SIGTERM ended the Wait context: the library withdrew the ticket
	exitCancelled = 4 // a cancel request was found
	exitOrphaned  = 5 // the harness's pipe closed
)

// waiterMain is one waiter: Open (the state directory from CORESLOT_STATE_DIR, with its
// config.toml), Enqueue cpu=1, then Wait, the library's polling loop. Admitted, it closes the
// grant and exits. It writes "E <pid>" once enqueued and, on SIGUSR1, "B <pid> <cpu-ns>": its
// own CPU so far, the baseline the harness subtracts. SIGTERM withdraws. Stdin is the harness's
// pipe: its end means the harness is gone.
func waiterMain(args []string) int {
	fs := flag.NewFlagSet("waiter", flag.ContinueOnError)
	label := fs.String("label", "", "ticket label")
	profPath := fs.String("cpuprofile", "", "profile from SIGUSR1 to this path")
	profFor := fs.Duration("profile-for", time.Minute, "profile length")
	profHz := fs.Int("profile-hz", 500, "profile sampling rate")
	if err := fs.Parse(args); err != nil {
		return exitError
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGUSR1, syscall.SIGTERM)
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(exitOrphaned)
	}()
	pid := os.Getpid()

	c, err := coreslot.Open(coreslot.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "waiter %d: open: %v\n", pid, err)
		return exitError
	}
	w, err := c.Enqueue(context.Background(), coreslot.Request{Label: *label,
		Need: map[string]coreslot.Range{"cpu": {Min: 1, Max: 1}}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "waiter %d: enqueue: %v\n", pid, err)
		return exitError
	}
	fmt.Fprintf(os.Stdout, "E %d\n", pid) // one write, under PIPE_BUF: atomic on the shared pipe

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		g   *coreslot.Grant
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		g, err := w.Wait(ctx)
		done <- outcome{g, err}
	}()

	var profFile *os.File
	stopProfile := func() {
		if profFile != nil {
			pprof.StopCPUProfile()
			profFile.Close()
			profFile = nil
		}
	}
	defer stopProfile()
	var profEnd <-chan time.Time
	for {
		select {
		case s := <-sigs:
			if s == syscall.SIGTERM {
				cancel()
				continue
			}
			fmt.Fprintf(os.Stdout, "B %d %d\n", pid, selfCPU())
			if *profPath != "" && profFile == nil && profEnd == nil {
				f, err := os.Create(*profPath)
				if err != nil {
					fmt.Fprintf(os.Stderr, "waiter %d: profile: %v\n", pid, err)
					continue
				}
				runtime.SetCPUProfileRate(*profHz) // StartCPUProfile then warns and keeps this rate
				if err := pprof.StartCPUProfile(f); err != nil {
					fmt.Fprintf(os.Stderr, "waiter %d: profile: %v\n", pid, err)
					f.Close()
					continue
				}
				profFile = f
				profEnd = time.After(*profFor)
			}
		case <-profEnd:
			stopProfile()
		case o := <-done:
			stopProfile()
			switch {
			case o.err == nil:
				if err := o.g.Close(); err != nil {
					fmt.Fprintf(os.Stderr, "waiter %d: close: %v\n", pid, err)
				}
				return exitAdmitted
			case errors.Is(o.err, coreslot.ErrCanceled):
				return exitCancelled
			case errors.Is(o.err, context.Canceled):
				return exitWithdrawn
			default:
				fmt.Fprintf(os.Stderr, "waiter %d: wait: %v\n", pid, o.err)
				return exitError
			}
		}
	}
}

// leaderMain is the process-group leader: it only waits for the harness's pipe to close.
func leaderMain() int {
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

// selfCPU is this process's user plus system CPU so far, in nanoseconds.
func selfCPU() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return -1
	}
	return ru.Utime.Nano() + ru.Stime.Nano()
}
