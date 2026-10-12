//go:build coreslot_testhooks && unix

// Command libclient is the kill-storm's library client: a process that queues through the Go
// library (Open, Enqueue, Wait) so that the storm can kill, stop and cancel a library waiter or
// holder the way it does a CLI run. Once admitted it either reports to MARKDIR/NAME.started and
// holds the grant until MARKDIR/NAME.release exists, or runs CMD with the grant's descriptors
// and environment (a nested run, when CMD is a coreslot run; CMD reports itself) and closes the
// grant when CMD exits. It never outlives the process that started it for long: it exits as
// soon as it is reparented, and every wait has a hard deadline.
//
//	libclient -dir STATEDIR -mark MARKDIR -name NAME -label LABEL -need RES=MIN..MAX... \
//	    [-priority P] [-timeout D] [-- CMD...]
//
// Exit status: 0 after a grant was released, 75 when the wait was cancelled or timed out, 70 on
// any other error, 4 when reparented.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Nezhinskiy/coreslot"
)

const deadline = 5 * time.Minute

type needs map[string]coreslot.Range

func (n needs) String() string { return fmt.Sprint(map[string]coreslot.Range(n)) }

func (n needs) Set(v string) error {
	name, rng, ok := strings.Cut(v, "=")
	lo, hi, ok2 := strings.Cut(rng, "..")
	if !ok || !ok2 {
		return fmt.Errorf("need %q is not RES=MIN..MAX", v)
	}
	a, err1 := strconv.Atoi(lo)
	b, err2 := strconv.Atoi(hi)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("need %q is not RES=MIN..MAX", v)
	}
	n[name] = coreslot.Range{Min: a, Max: b}
	return nil
}

func main() {
	need := needs{}
	dir := flag.String("dir", "", "state directory")
	mark := flag.String("mark", "", "marker directory")
	name := flag.String("name", "", "marker name")
	label := flag.String("label", "", "request label")
	priority := flag.Int("priority", 0, "request priority")
	timeout := flag.Duration("timeout", 0, "withdraw after this long without admission (0: wait)")
	flag.Var(need, "need", "RES=MIN..MAX, repeatable")
	flag.Parse()
	cmd := flag.Args()

	ppid := os.Getppid()
	ctx, stop := context.WithTimeout(context.Background(), deadline)
	defer stop()
	go func() { // reparented: the storm is gone
		for range time.Tick(20 * time.Millisecond) {
			if os.Getppid() != ppid {
				os.Exit(4)
			}
		}
	}()

	c, err := coreslot.Open(coreslot.Options{StateDir: *dir})
	if err != nil {
		fail(err)
	}
	w, err := c.Enqueue(coreslot.Request{Label: *label, Priority: *priority, Need: need})
	if err != nil {
		fail(err)
	}
	wctx := ctx
	if *timeout > 0 {
		var cancel context.CancelFunc
		wctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	g, err := w.Wait(wctx)
	if errors.Is(err, coreslot.ErrCancelled) || errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintln(os.Stderr, "libclient: withdrawn:", err)
		os.Exit(75)
	}
	if err != nil {
		fail(err)
	}
	if len(cmd) > 0 {
		run := exec.Command(cmd[0], cmd[1:]...)
		run.Stdout, run.Stderr = os.Stdout, os.Stderr
		run.ExtraFiles = g.Files()
		run.Env = append(withoutGrantContext(os.Environ()), g.Env()...)
		if err := run.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "libclient: command:", err)
		}
	} else {
		report, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "grant": g.Grant()})
		writeAtomic(filepath.Join(*mark, *name+".started"), report)
		waitFor(ctx, filepath.Join(*mark, *name+".release"))
	}
	if err := g.Close(); err != nil {
		fail(err)
	}
}

// withoutGrantContext drops inherited grant context, as Grant.Env asks of its caller.
func withoutGrantContext(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CORESLOT_GRANTED_") || k == "CORESLOT_HELD_FD" ||
			k == "CORESLOT_TOKEN_FDS" || k == "CORESLOT_UNBUDGETED" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func waitFor(ctx context.Context, path string) {
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "libclient: deadline passed waiting for", path)
			os.Exit(3)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func writeAtomic(path string, data []byte) {
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		fail(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "libclient:", err)
	os.Exit(70)
}
