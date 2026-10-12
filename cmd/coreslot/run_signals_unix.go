//go:build unix

package main

import (
	"context"
	"os"
	"syscall"
)

// runSignals receives the signals a run handles from its start: while it waits they interrupt
// the wait, and while its command runs the supervisor decides what reaches the command.
var runSignals = make(chan os.Signal, 16)

// handledSignals are caught for the whole run, unless the run started with one ignored. A
// caught disposition does not survive exec, so the command starts with the default ones.
var handledSignals = []os.Signal{syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP}

// sigWatch turns a signal received while the run waits into the end of the wait.
type sigWatch struct {
	stop, done chan struct{}
	sig        os.Signal
}

func watchSignals(cancel context.CancelFunc) *sigWatch {
	w := &sigWatch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		select {
		case w.sig = <-runSignals:
			cancel()
		case <-w.stop:
		}
	}()
	return w
}

// end stops watching and returns the signal that ended the wait, or nil.
func (w *sigWatch) end() os.Signal {
	close(w.stop)
	<-w.done
	return w.sig
}

// pendingSignal returns a handled signal the run received and has not acted on, or 0.
func pendingSignal() syscall.Signal {
	select {
	case s := <-runSignals:
		return s.(syscall.Signal)
	default:
		return 0
	}
}
