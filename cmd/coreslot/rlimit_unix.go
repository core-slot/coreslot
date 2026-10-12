//go:build unix

package main

import (
	"bytes"
	"context"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/testhooks"
)

// extraNofile is how far a run raises the soft RLIMIT_NOFILE it passes to its command: a grant
// adds up to 513 descriptors (PROTOCOL.md section 6).
const extraNofile = 256

// Bounds of the descriptor-limit probe. A probe that has not printed within probeTimeout is
// killed and the run falls back; probeWaitDelay bounds the wait for its output pipe after that.
const (
	probeTimeout   = time.Second
	probeWaitDelay = 100 * time.Millisecond
)

// The Go runtime raises its own soft RLIMIT_NOFILE at start-up, keeps the original in a private
// variable, and restores that original in every child it starts (on Linux, while its own is still
// the raised one) until the program calls syscall.Setrlimit for RLIMIT_NOFILE, which drops it.
// getrlimit therefore reports the raised limit, not the one the caller gave coreslot. A child
// started before that call still gets the original, so a nofileProbe learns it from
// `/bin/sh -c 'ulimit -S -n'` started then.
//
// A run starts the probe before it queues and collects it just before its command starts, so the
// few milliseconds the probe costs overlap with admission.

// nofileProbe is a running or finished probe of the soft RLIMIT_NOFILE this process was started
// with. Its methods are for one goroutine; a nil *nofileProbe has learned nothing.
type nofileProbe struct {
	done     chan struct{}
	cancel   context.CancelFunc // kills the probe; nil when none was started
	soft     uint64
	ok       bool
	shielded []shieldedFD
}

// shieldedFD is a descriptor the probe must not inherit, with its descriptor flags before
// close-on-exec was set on it.
type shieldedFD struct{ fd, flags int }

// startNofileProbe starts learning the original soft RLIMIT_NOFILE. It must run before the
// process calls syscall.Setrlimit for RLIMIT_NOFILE (raiseChildNofile), or the probe reports the
// limit that call set. shield names descriptors the probe must not inherit although they are not
// close-on-exec: a pass-through's inherited grant (PROTOCOL.md section 18), which it passes on to
// its command at the same numbers. Close-on-exec is set on them until wait, so nothing else may
// start a process meanwhile. Every descriptor this process opens itself, its own grant's among
// them, is close-on-exec, so an ordinary run's probe needs no shield.
func startNofileProbe(shield []int) *nofileProbe {
	p := &nofileProbe{done: make(chan struct{})}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		close(p.done)
		return p
	}
	if cur := uint64(lim.Cur); !runtimeMayHaveRaised(cur, uint64(lim.Max)) {
		p.soft, p.ok = cur, true
		close(p.done)
		return p
	}
	p.shielded = setCloseOnExec(shield)
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	p.cancel = cancel
	go func() {
		defer close(p.done)
		p.soft, p.ok = probeSoftNofile(ctx)
	}()
	return p
}

// runtimeMayHaveRaised reports whether the soft limit cur, under the hard limit hard, may be one
// the Go runtime set at start-up rather than the one this process was started with. The runtime
// (syscall/rlimit.go) raises only when hard > 0 and the original is below hard-1, and then to
// hard-1, which on macOS it lowers to kern.maxfilesperproc (syscall/rlimit_darwin.go), the
// fileCap here. Its setrlimit may also fail, as Linux's does above fs.nr_open when the hard limit
// is unlimited, which leaves the original. So a soft limit that is neither hard-1 nor the cap is
// the original one, and needs no probe. One that is may still be the original, which the probe
// then reports.
func runtimeMayHaveRaised(cur, hard uint64) bool {
	if hard == 0 {
		return false
	}
	c := fileCap()
	return cur == hard-1 || (c > 0 && cur == c)
}

// probeSoftNofile runs the probe until ctx ends and parses what it printed: a decimal number or
// "unlimited". The probe gets no environment, so no startup file runs, and its standard input
// and error are the null device.
func probeSoftNofile(ctx context.Context) (uint64, bool) {
	cmd := exec.CommandContext(ctx, testhooks.ProbeShell(), "-c", "ulimit -S -n")
	cmd.Env = []string{}
	cmd.WaitDelay = probeWaitDelay
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0, false
	}
	s := strings.TrimSpace(out.String())
	if s == "unlimited" {
		return math.MaxUint64, true
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// wait waits for the probe to end, restores the shielded descriptors' flags, and returns the
// original soft limit; ok is false when the probe failed (it could not start, was killed after
// probeTimeout, or printed something else), and the caller falls back to the limit it observes.
func (p *nofileProbe) wait() (soft uint64, ok bool) {
	if p == nil {
		return 0, false
	}
	<-p.done
	if p.cancel != nil {
		p.cancel()
	}
	for _, s := range p.shielded {
		_, _ = unix.FcntlInt(uintptr(s.fd), unix.F_SETFD, s.flags)
	}
	p.shielded = nil
	return p.soft, p.ok
}

// stop kills a probe still running and reaps it, so that no probe outlives the run that started
// it or is left a zombie. It is what ends a probe that a run which never started its command
// (it refused or was interrupted) did not collect.
func (p *nofileProbe) stop() {
	if p != nil && p.cancel != nil {
		p.cancel()
	}
	_, _ = p.wait()
}

// setCloseOnExec sets close-on-exec on each open descriptor of fds from 3 that lacks it and
// returns those it changed, with their previous flags.
func setCloseOnExec(fds []int) []shieldedFD {
	var changed []shieldedFD
	for _, fd := range fds {
		if fd < 3 {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC != 0 {
			continue
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err == nil {
			changed = append(changed, shieldedFD{fd, flags})
		}
	}
	return changed
}

// originalNofile is the soft RLIMIT_NOFILE this process was started with, as the probe learned
// it, or else the one it observes.
func (p *nofileProbe) originalNofile() uint64 {
	if soft, ok := p.wait(); ok {
		return soft
	}
	return currentNofile()
}

// raiseChildNofile sets the soft RLIMIT_NOFILE the command inherits to original + 256, within the
// hard limit (and, on macOS, within kern.maxfilesperproc, above which setrlimit fails), and
// returns the soft limit set. original is the soft limit coreslot was started with; inherited is
// how many descriptors the command gets at fixed numbers from 0 (the standard streams and the
// grant from 3), or 0 when it gets none that way. A command never gets less than inherited + 256:
// below inherited the run could not even place the grant's descriptors at their numbers.
//
// It must go through syscall.Setrlimit: the Go runtime restores the original limit in every child
// it starts unless the program sets the limit itself through that function. After this call a
// new probe would report the limit set here, so a probe must have ended before it.
func raiseChildNofile(original uint64, inherited int) (soft uint64, err error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, err
	}
	base := max(original, uint64(max(inherited, 0)))
	if base > math.MaxUint64-extraNofile {
		soft = math.MaxUint64
	} else {
		soft = base + extraNofile
	}
	soft = min(soft, uint64(lim.Max))
	if c := fileCap(); c > 0 {
		soft = min(soft, c)
	}
	setLimit(&lim.Cur, soft)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, err
	}
	return soft, nil
}

// currentNofile is the soft RLIMIT_NOFILE this process observes: the limit the Go runtime raised
// it to, which is the fallback when the probe failed.
func currentNofile() uint64 {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0
	}
	return uint64(lim.Cur)
}

// setLimit stores v in an rlimit field, which is uint64 on some systems and int64 on others.
func setLimit[T int64 | uint64](field *T, v uint64) { *field = T(v) }
