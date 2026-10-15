//go:build unix

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot"
)

// libraryHelper runs the helpers that use the Go library from inside a run's command.
func libraryHelper(name string, args []string) (code int, ok bool) {
	if name != "lib-wait" {
		return 0, false
	}
	return libWait(args), true
}

// libWait is a Go program inside `coreslot run` that queues through the library: args are the
// cpu min and max, how long Wait may take, and "independent" or "nested" for
// Options.Independent. It prints the grant (or the error) with how long the Enqueue and Wait
// took, and what queue/ and holds/ held and gen read while it had the grant.
func libWait(args []string) int {
	if len(args) != 4 {
		fmt.Fprintln(os.Stderr, "lib-wait: want MIN MAX WAIT independent|nested")
		return 2
	}
	lo, _ := strconv.Atoi(args[0])
	hi, _ := strconv.Atoi(args[1])
	wait, err := time.ParseDuration(args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "lib-wait:", err)
		return 2
	}
	dir := os.Getenv("CORESLOT_STATE_DIR")
	gen := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "gen"))
		return strings.TrimSpace(string(b))
	}
	names := func(sub string) string {
		des, _ := os.ReadDir(filepath.Join(dir, sub))
		var out []string
		for _, de := range des {
			out = append(out, de.Name())
		}
		return strings.Join(out, ",")
	}
	before := gen()
	c, err := coreslot.Open(coreslot.Options{Independent: args[3] == "independent"})
	if err != nil {
		fmt.Println("error:", err)
		return 1
	}
	start := time.Now()
	w, err := c.Enqueue(context.Background(), coreslot.Request{Label: "lib", Need: map[string]coreslot.Range{"cpu": {Min: lo, Max: hi}}})
	if err != nil {
		fmt.Println("error:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	g, err := w.Wait(ctx)
	took := time.Since(start)
	if err != nil {
		fmt.Printf("error: %v (after %v)\nqueue [%s]\n", err, took.Round(10*time.Millisecond), names("queue"))
		return 1
	}
	defer g.Close()
	fmt.Printf("grant cpu=%d passthrough=%v after %v\n", g.Granted()["cpu"], g.PassThrough(), took.Round(10*time.Millisecond))
	fmt.Printf("queue [%s]\nholds [%s]\ngen %s -> %s\n", names("queue"), names("holds"), before, gen())
	if took > 2*time.Second {
		fmt.Println("slow")
	}
	return 0
}

// The defect a library caller inside a run hit: budget 2, `coreslot run --need cpu=2` runs a Go
// program that asks the library for cpu 1..2 and waits up to 10 s. Before pass-through it queued
// behind its own parent and timed out (it would have waited forever without a bound); now it
// runs at once on the parent's grant, with no ticket, no hold of its own and no gen bump. With
// Options.Independent it queues as an unrelated request and waits again. Mutation: making
// Enqueue ignore the inherited context (always queue) reddens "pass-through" with the
// timeout of the defect.
func TestLibraryInsideRunPassesThrough(t *testing.T) {
	t.Run("pass-through", func(t *testing.T) {
		dir := stateDir(t, "[resources]\ncpu = 2\n")
		r := cli(t, baseEnv(dir), append([]string{"run", "--need", "cpu=2", "--"}, helper(t, "lib-wait", "1", "2", "10s", "nested")...)...)
		t.Logf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
		if r.code != 0 {
			t.Fatalf("the library inside the run got no grant: exit %d, %s", r.code, strings.TrimSpace(r.stdout))
		}
		if !strings.Contains(r.stdout, "grant cpu=2 passthrough=true after ") || strings.Contains(r.stdout, "slow") {
			t.Errorf("stdout %q: want a pass-through grant of cpu 2 at once", r.stdout)
		}
		lines := strings.Split(r.stdout, "\n")
		if len(lines) < 4 || lines[1] != "queue []" || strings.Count(lines[2], ",") != 0 || lines[2] == "holds []" {
			t.Errorf("stdout %q: want no ticket and one hold, the parent's", r.stdout)
		}
		if f := strings.Fields(lines[3]); len(f) != 4 || f[1] != f[3] {
			t.Errorf("%q: the pass-through bumped gen", lines[3])
		}
		tokensFree(t, dir, 2)
	})
	t.Run("independent", func(t *testing.T) {
		dir := stateDir(t, "[resources]\ncpu = 2\n")
		r := cli(t, baseEnv(dir), append([]string{"run", "--need", "cpu=2", "--"}, helper(t, "lib-wait", "1", "2", "1s", "independent")...)...)
		t.Logf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
		if r.code != 1 || !strings.Contains(r.stdout, "error: context deadline exceeded") {
			t.Fatalf("exit %d, stdout %q: an Independent request must queue behind the parent until its bound", r.code, r.stdout)
		}
		if !strings.Contains(r.stdout, "queue []") {
			t.Errorf("stdout %q: the abandoned Wait must have withdrawn its ticket", r.stdout)
		}
	})
}
