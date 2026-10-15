//go:build unix && coreslot_testhooks

package coreslot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func init() { moreHelpers["named"] = passThroughNamedChild }

// A Client opened without Options.StateDir is on the state directory the environment names by
// construction, so its pass-through decides without resolving that directory again: a second
// home-directory lookup may run getent again and fail where Open's succeeded (a timeout), and the
// request must not then be taken for another budget's and queue behind its own parent, which
// holds the whole budget here. The child of the test binary runs inside the parent's grant with
// no CORESLOT_STATE_DIR, the password database made to miss, and a getent substitute that answers
// its first run only. Mutation: decide through DecideNested, which resolves the directory again,
// instead of DecideNestedNamed (the child's request queues behind the parent, and getent runs a
// second time).
func TestPassThroughDoesNotResolveTheDirectoryAgain(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "state", "coreslot")
	if runtime.GOOS == "darwin" {
		dir = filepath.Join(home, "Library", "Application Support", "coreslot")
	}
	cfgDir := filepath.Join(home, ".config", "coreslot")
	for _, d := range []string{dir, cfgDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The parent's budget and the child's view of it: one cpu, which the parent holds.
	for _, p := range []string{filepath.Join(dir, "config.toml"), filepath.Join(cfgDir, "config.toml")} {
		if err := os.WriteFile(p, []byte("[resources]\ncpu = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	scratch := t.TempDir()
	runs := filepath.Join(scratch, "runs")
	getent := filepath.Join(scratch, "getent")
	script := `#!/bin/sh
[ "$1" = passwd ] || exit 2
if [ -e "$GETENT_RUNS" ]; then echo run >>"$GETENT_RUNS"; exit 1; fi
echo run >>"$GETENT_RUNS"
echo "me:x:$2:$2:me:$GETENT_HOME:/bin/sh"
`
	if err := os.WriteFile(getent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	g := waitGrant(t, open(t, dir), Request{Label: "parent", Need: map[string]Range{"cpu": {Min: 1, Max: 1}}})
	f := g.Files() // hold, cpu/0
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.ExtraFiles = f
	cmd.Env = append(withoutCoreslot(), nestedEnv+"=named", "CORESLOT_HELD_FD=3", "CORESLOT_TOKEN_FDS=4",
		"CORESLOT_TESTHOOK_PASSWD_MISS=1", "CORESLOT_TESTHOOK_GETENT="+getent,
		"GETENT_HOME="+home, "GETENT_RUNS="+runs, "WANT_STATE_DIR="+dir)
	out, err := cmd.CombinedOutput()
	t.Logf("child:\n%s", out)
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	b, err := os.ReadFile(runs)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "run\n"); n != 1 {
		t.Errorf("getent ran %d times, want once, for Open", n)
	}
}

// passThroughNamedChild is the process inside the grant of
// TestPassThroughDoesNotResolveTheDirectoryAgain.
func passThroughNamedChild() error {
	c, err := Open(Options{})
	if err != nil {
		return err
	}
	if got, want := c.e.Store().Dir(), os.Getenv("WANT_STATE_DIR"); got != want {
		return fmt.Errorf("Open resolved %q, want %q", got, want)
	}
	w, err := c.Enqueue(context.Background(), Request{Need: map[string]Range{"cpu": {Min: 1, Max: 1}}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	g, err := w.Wait(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		_ = w.Cancel()
		return fmt.Errorf("the request queued behind its own parent, which holds the whole budget")
	}
	if err != nil {
		return err
	}
	defer g.Close()
	if !g.PassThrough() || g.Granted()["cpu"] != 1 {
		return fmt.Errorf("grant %v, pass-through %v; want the parent's cpu, passed through", g.Granted(), g.PassThrough())
	}
	fmt.Println("named: ok")
	return nil
}
