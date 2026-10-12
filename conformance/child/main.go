//go:build coreslot_testhooks && unix

// Command child is the conformance suite's helper process. It is the command a coreslot run
// starts, reporting what it inherited and holding its grant until the test releases it, and a
// direct protocol client for the steps the CLI does not offer (publishing a cancel request,
// a forced reconciliation, sitting inside the admission section). It is built only with the
// coreslot_testhooks tag, like the binaries it runs beside, and every mode has a hard deadline
// so that no helper outlives a failed test for long.
//
//	child hold MARKDIR NAME          report to MARKDIR/NAME.started, exit 0 once MARKDIR/NAME.release exists
//	child cancel STATEDIR TICKET-ID  publish a cancel request for TICKET-ID
//	child reconcile STATEDIR OFFSET  reconcile now, as a clock OFFSET (a Go duration) ahead would
//	child section STATEDIR MARKER    enter the admission section, create MARKER, stay until MARKER.release exists
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Nezhinskiy/coreslot/internal/config"
	"github.com/Nezhinskiy/coreslot/internal/engine"
	"github.com/Nezhinskiy/coreslot/internal/store"
	"github.com/Nezhinskiy/coreslot/internal/testhooks"
	"github.com/Nezhinskiy/coreslot/internal/wire"
)

// deadline bounds every mode: a helper whose test died never lingers longer than this.
const deadline = 5 * time.Minute

// Report is what `hold` writes: its pid, the CORESLOT_* environment it inherited and every
// open descriptor from 3 on, with its device and inode.
type Report struct {
	PID  int               `json:"pid"`
	Env  map[string]string `json:"env"`
	FDs  map[int][2]uint64 `json:"fds"`
	Args []string          `json:"args"`
}

func main() {
	// The descriptors are read before this process opens anything, so that none of its own
	// lands among them.
	fds := openFDs()
	if len(os.Args) < 2 {
		fail("usage: child hold|cancel|reconcile|section ...")
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "hold":
		need(args, 2)
		hold(args[0], args[1], fds)
	case "cancel":
		need(args, 2)
		cancel(args[0], args[1])
	case "reconcile":
		need(args, 2)
		reconcile(args[0], args[1])
	case "section":
		need(args, 2)
		section(args[0], args[1])
	default:
		fail("unknown mode " + os.Args[1])
	}
}

func need(args []string, n int) {
	if len(args) != n {
		fail(fmt.Sprintf("%s takes %d arguments, got %d", os.Args[1], n, len(args)))
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "child:", msg)
	os.Exit(2)
}

// openFDs lists the open descriptors from 3 on. On Linux the Go runtime itself keeps the
// cgroup's cpu.max open (for its GOMAXPROCS) from before main runs; that descriptor is its own,
// not inherited, and is left out.
func openFDs() map[int][2]uint64 {
	fds := map[int][2]uint64{}
	for fd := 3; fd <= 600; fd++ {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil {
			continue
		}
		if l, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd)); err == nil && strings.HasPrefix(l, "/sys/fs/cgroup/") {
			continue
		}
		fds[fd] = [2]uint64{uint64(st.Dev), uint64(st.Ino)}
	}
	return fds
}

// writeAtomic creates path with data through a rename, so that a reader never sees it half
// written.
func writeAtomic(path string, data []byte) {
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		fail(err.Error())
	}
	if err := os.Rename(tmp, path); err != nil {
		fail(err.Error())
	}
}

// waitFor returns once path exists. It exits 3 at the deadline, and 4 when the process that
// started this one is gone (reparented), so that a helper never outlives its run.
func waitFor(path string) {
	ppid := os.Getppid()
	end := time.Now().Add(deadline)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if os.Getppid() != ppid {
			os.Exit(4)
		}
		if time.Now().After(end) {
			fmt.Fprintln(os.Stderr, "child: deadline passed waiting for", path)
			os.Exit(3)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func hold(markDir, name string, fds map[int][2]uint64) {
	r := Report{PID: os.Getpid(), Env: map[string]string{}, FDs: fds, Args: os.Args}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "CORESLOT_") &&
			!strings.HasPrefix(k, "CORESLOT_TESTHOOK_") {
			r.Env[k] = v
		}
	}
	data, err := json.Marshal(r)
	if err != nil {
		fail(err.Error())
	}
	writeAtomic(filepath.Join(markDir, name+".started"), data)
	waitFor(filepath.Join(markDir, name+".release"))
}

func open(dir string) (*engine.Engine, *store.Store) {
	st, err := store.Open(dir)
	if err != nil {
		fail(err.Error())
	}
	e := engine.New(st, &config.Watcher{Path: filepath.Join(dir, store.ConfigFile)}, config.MachineCPUs, engine.Clock{})
	return e, st
}

func cancel(dir, idText string) {
	id, ok := wire.ParseID(idText)
	if !ok {
		fail("not a ticket id: " + idText)
	}
	e, _ := open(dir)
	err := e.Cancel(id)
	switch {
	case err == nil:
		fmt.Println("cancel requested")
	case errors.Is(err, engine.ErrNoTicket):
		fmt.Println("no such ticket")
		os.Exit(5)
	default:
		fail(err.Error())
	}
}

func reconcile(dir, offsetText string) {
	offset, err := time.ParseDuration(offsetText)
	if err != nil {
		fail(err.Error())
	}
	_, st := open(dir)
	ctx, stop := context.WithTimeout(context.Background(), time.Minute)
	defer stop()
	sec, err := st.Enter(ctx)
	if err != nil {
		fail(err.Error())
	}
	defer sec.Leave()
	if _, understood, err := sec.Format(testhooks.Formats()); err != nil || !understood {
		fail(fmt.Sprintf("FORMAT not understood (%v)", err))
	}
	removed, err := sec.ReconcileIfDue(store.NowNS() + int64(offset))
	if err != nil {
		fail(err.Error())
	}
	fmt.Println("removed", strconv.Itoa(removed))
}

func section(dir, marker string) {
	_, st := open(dir)
	ctx, stop := context.WithTimeout(context.Background(), time.Minute)
	defer stop()
	sec, err := st.Enter(ctx)
	if err != nil {
		fail(err.Error())
	}
	defer sec.Leave()
	if _, _, err := sec.Format(testhooks.Formats()); err != nil {
		fail(err.Error())
	}
	writeAtomic(marker, []byte(strconv.Itoa(os.Getpid())))
	waitFor(marker + ".release")
}
