//go:build unix

package main

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/core-slot/coreslot/internal/testhooks"
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
	done   chan struct{}
	cancel context.CancelFunc // kills the probe; nil when none was started
	soft   uint64
	ok     bool
}

// startNofileProbe starts learning the original soft RLIMIT_NOFILE. It must run before the
// process calls syscall.Setrlimit for RLIMIT_NOFILE (raiseChildNofile), or the probe reports the
// limit that call set. The probe inherits no grant: every descriptor this process opens itself,
// its own grant's among them, is close-on-exec, and so is an inherited grant until the command
// starts (shieldInherited).
func startNofileProbe() *nofileProbe {
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
	ctx, cancel := context.WithTimeout(context.Background(), testhooks.ProbeTimeout(probeTimeout))
	p.cancel = cancel
	// The shell is started here, not in the goroutine: Start returns only once the forked child
	// has exec'd, which closed its copies of this process's close-on-exec descriptors. Forked
	// concurrently, the child could still share the description of the ticket the run publishes
	// next, and the run's own probe of that ticket, once admitted, would find it locked and leave
	// it in queue/ for a reconciliation instead of unlinking it (PROTOCOL.md section 12.1 step 7).
	// Only the shell's run overlaps with admission.
	cmd, out := probeCommand(ctx)
	// A test pauses here to check that the run publishes nothing before the probe has exec'd.
	testhooks.Point("probe.before-start")
	if err := cmd.Start(); err != nil {
		close(p.done)
		return p
	}
	go func() {
		defer close(p.done)
		p.soft, p.ok = parseProbe(cmd.Wait(), out)
	}()
	return p
}

// runtimeMayHaveRaised reports whether the soft limit cur, under the hard limit hard, may be one
// the Go runtime set at start-up rather than the one this process was started with. The runtime
// (syscall/rlimit.go) raises only when hard > 0 and the original is below hard-1, and then to
// hard-1, which on macOS it lowers to kern.maxfilesperproc (syscall/rlimit_darwin.go), the
// fileCap here. Its setrlimit may also fail, which leaves the original: Linux checks the hard limit
// a call passes against fs.nr_open on every call, so it refuses to set any limit once fs.nr_open
// was lowered below a hard limit inherited from before, and a sandbox may refuse the call. (A
// Linux hard limit is never unlimited: setrlimit refuses one above fs.nr_open.) So a soft limit
// that is neither hard-1 nor the cap is the original one, and needs no probe. One that is may
// still be the original, which the probe then reports.
func runtimeMayHaveRaised(cur, hard uint64) bool {
	if hard == 0 {
		return false
	}
	c := fileCap()
	return cur == hard-1 || (c > 0 && cur == c)
}

// probeCommand is the probe: `sh -c 'ulimit -S -n'`, killed when ctx ends. It gets no
// environment, so no startup file runs, and its standard input and error are the null device;
// what it prints goes to out.
func probeCommand(ctx context.Context) (*exec.Cmd, *bytes.Buffer) {
	cmd := exec.CommandContext(ctx, testhooks.ProbeShell(), "-c", "ulimit -S -n")
	cmd.Env = []string{}
	cmd.WaitDelay = probeWaitDelay
	out := &bytes.Buffer{}
	cmd.Stdout = out
	return cmd, out
}

// parseProbe parses what a probe that ended with err printed: a decimal number or "unlimited".
func parseProbe(err error, out *bytes.Buffer) (uint64, bool) {
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(out.String())
	if s == "unlimited" {
		return math.MaxUint64, true
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// wait waits for the probe to end and returns the original soft limit; ok is false when the probe
// failed (it could not start, was killed after probeTimeout, or printed something else), and the
// caller falls back to the limit it observes.
func (p *nofileProbe) wait() (soft uint64, ok bool) {
	if p == nil {
		return 0, false
	}
	<-p.done
	if p.cancel != nil {
		p.cancel()
	}
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

// originalNofile is the soft RLIMIT_NOFILE this process was started with, as the probe learned
// it, or else the one it observes.
func (p *nofileProbe) originalNofile() uint64 {
	if soft, ok := p.wait(); ok {
		return soft
	}
	return currentNofile()
}

// raiseChildNofile sets the soft RLIMIT_NOFILE before the command starts and returns the soft
// limit set (childNofile). original is the soft limit coreslot was started with; inherited is how
// many descriptors the command gets at fixed numbers from 0 (the standard streams and ExtraFiles
// from fd 3), or 0 when it gets none that way.
//
// The limit is coreslot's own as much as the command's: coreslot creates the exec-status pipe and
// places ExtraFiles at their numbers under it, and opens the run log under it once the command
// exited. So it is measured against the descriptors coreslot has open just before the call,
// listed in fdDir: the standard streams, whatever it inherited, the grant it holds or, as a
// pass-through, passes on by inheritance, and the runtime's own.
//
// It must go through syscall.Setrlimit: the Go runtime restores the original limit in every child
// it starts unless the program sets the limit itself through that function. After this call a
// new probe would report the limit set here, so a probe must have ended before it.
func raiseChildNofile(original uint64, inherited int) (soft uint64, err error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, err
	}
	highest, listed := highestOpenFD()
	soft = childNofile(original, inherited, highest, listed, uint64(lim.Cur), uint64(lim.Max))
	setLimit(&lim.Cur, soft)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, err
	}
	return soft, nil
}

// childNofile is the soft RLIMIT_NOFILE raiseChildNofile sets:
//
//	min(max(original, highest+1, inherited) + 256, hard, kern.maxfilesperproc on macOS)
//
// where highest is the highest descriptor this process has open (highestOpenFD). So coreslot
// never lowers its own soft limit below the descriptors it already has open plus 256, and a
// pass-through, whose command inherits the grant at the numbers the pass-through holds it, gives
// that command at least the grant's highest descriptor plus 257. When the descriptors could not be
// listed (listed is false), current, the soft limit this process observes, which the Go runtime
// raised at start-up, takes the place of highest+1: the limit is not lowered at all, as before the
// original limit was learned. The cap applies because macOS refuses a soft limit above
// kern.maxfilesperproc.
func childNofile(original uint64, inherited, highest int, listed bool, current, hard uint64) uint64 {
	floor := uint64(max(inherited, 0))
	if listed {
		floor = max(floor, uint64(highest)+1)
	} else {
		floor = max(floor, current)
	}
	base := max(original, floor)
	soft := uint64(math.MaxUint64)
	if base <= math.MaxUint64-extraNofile {
		soft = base + extraNofile
	}
	soft = min(soft, hard)
	if c := fileCap(); c > 0 {
		soft = min(soft, c)
	}
	return soft
}

// highestOpenFD is the highest descriptor this process has open, read from the listing of fdDir;
// ok is false when no listing is known on this platform or it cannot be read. The listing's own
// descriptor is among those it lists, which can only raise the answer.
func highestOpenFD() (highest int, ok bool) {
	if fdDir == "" {
		return 0, false
	}
	f, err := os.Open(fdDir)
	if err != nil {
		return 0, false
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return 0, false
	}
	highest = -1
	for _, name := range names {
		if fd, err := strconv.Atoi(name); err == nil && fd > highest {
			highest = fd
		}
	}
	return highest, highest >= 0
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
