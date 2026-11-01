//go:build unix

package coreslot_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/core-slot/coreslot"
)

// The examples open a budget of their own in a temporary directory, so that they run anywhere
// and leave the machine's budget alone; a program opens coreslot.Options{} instead, the budget
// every other client of the user shares.

// Queue for one memory token, wait for it, and run a command inside the grant: the command inherits
// the grant's descriptors and learns what it may use from CORESLOT_GRANTED_MEM.
func ExampleClient_Enqueue() {
	dir, err := os.MkdirTemp("", "coreslot-example")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	c, err := coreslot.Open(coreslot.Options{StateDir: dir})
	if err != nil {
		log.Fatal(err) // a *coreslot.StateError: the budget cannot be used
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	w, err := c.Enqueue(ctx, coreslot.Request{Label: "example",
		Need: map[string]coreslot.Range{"mem": {Min: 1, Max: 1}}})
	if err != nil {
		log.Fatal(err)
	}
	g, err := w.Wait(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer g.Close()

	cmd := exec.CommandContext(ctx, "sh", "-c", `echo "the command may use $CORESLOT_GRANTED_MEM MiB"`)
	cmd.ExtraFiles, cmd.Env = g.Files(), g.Environ(os.Environ())
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		log.Fatal(err)
	}
	// Output: the command may use 128 MiB
}

// Poll instead of Wait, for a caller with a loop of its own: once at least every 2 s, or the
// other clients find the request STALLED and stop waiting for it.
func ExampleWaiter_Poll() {
	dir, err := os.MkdirTemp("", "coreslot-example")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	c, err := coreslot.Open(coreslot.Options{StateDir: dir})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	w, err := c.Enqueue(context.Background(), coreslot.Request{Label: "example"}) // mem 1..1
	if err != nil {
		log.Fatal(err)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		g, err := w.Poll()
		if err != nil {
			log.Fatal(err) // the request is still queued: Cancel withdraws it
		}
		if g != nil {
			fmt.Println("admitted with mem", g.Granted()["mem"])
			g.Close()
			return
		}
		<-tick.C // or do other work in between
	}
	// Output: admitted with mem 1
}

// Adopt work that is already running: it takes what is free, without queueing, and reports
// what the work runs without.
func ExampleClient_Adopt() {
	dir, err := os.MkdirTemp("", "coreslot-example")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	// A budget of two tokens, whatever the machine has.
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[resources]\nmem = \"256M\"\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	c, err := coreslot.Open(coreslot.Options{StateDir: dir})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	g, err := c.Adopt(context.Background(), coreslot.Request{Label: "already running",
		Need: map[string]coreslot.Range{"mem": {Min: 3, Max: 4}}})
	if err != nil {
		log.Fatal(err)
	}
	defer g.Close()
	fmt.Printf("granted %d, short of the minimum by %d\n", g.Granted()["mem"], g.Shortfall()["mem"])
	// Output: granted 2, short of the minimum by 1
}

// Read the budget, its holds and its queue, as `coreslot status` does.
func ExampleClient_Snapshot() {
	dir, err := os.MkdirTemp("", "coreslot-example")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[resources]\nmem = \"512M\"\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	c, err := coreslot.Open(coreslot.Options{StateDir: dir})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	g, err := c.Adopt(context.Background(), coreslot.Request{Label: "build",
		Need: map[string]coreslot.Range{"mem": {Min: 3, Max: 3}}})
	if err != nil {
		log.Fatal(err)
	}
	defer g.Close()

	r, err := c.Snapshot()
	if err != nil {
		log.Fatal(err)
	}
	mem := r.Resources["mem"]
	fmt.Printf("mem: size %d, locked %d, available %d\n", mem.Size, mem.Locked, mem.Available)
	for _, h := range r.Holds {
		fmt.Printf("%s holds mem %d (adopted %v)\n", h.Label, h.Granted["mem"], h.Adopted)
	}
	fmt.Println("queued:", len(r.Tickets))
	// Output:
	// mem: size 4, locked 3, available 1
	// build holds mem 3 (adopted true)
	// queued: 0
}

// Run a command inside the budget when there is one, and without it when the budget cannot be
// used: a *StateError (an unusable state directory, configuration or token) or ErrUnsupported (a
// platform without a budget) is the caller's to decide on, and this caller runs anyway. Every
// other error, its own context's included, stops it.
func Example_runUnbudgeted() {
	file, err := os.CreateTemp("", "coreslot-example")
	if err != nil {
		log.Fatal(err)
	}
	file.Close()
	defer os.Remove(file.Name())
	// A state directory that cannot be one: a path below a regular file.
	o := coreslot.Options{StateDir: filepath.Join(file.Name(), "state")}

	cmd := exec.Command("echo", "the command ran")
	cmd.Stdout = os.Stdout
	if err := runBudgeted(context.Background(), o, cmd); err != nil {
		log.Fatal(err)
	}
	// Output:
	// the budget cannot be used; running without it
	// the command ran
}

// runBudgeted runs cmd inside a grant of mem 1..1 from the budget o opens, or without the budget
// when it cannot be used.
func runBudgeted(ctx context.Context, o coreslot.Options, cmd *exec.Cmd) error {
	g, err := grant(ctx, o)
	var se *coreslot.StateError
	switch {
	case errors.As(err, &se), errors.Is(err, coreslot.ErrUnsupported):
		fmt.Println("the budget cannot be used; running without it")
		return cmd.Run()
	case err != nil:
		return err
	}
	defer g.Close()
	cmd.ExtraFiles, cmd.Env = g.Files(), g.Environ(os.Environ())
	return cmd.Run()
}

// grant opens the budget o names and waits for mem 1..1 in it.
func grant(ctx context.Context, o coreslot.Options) (*coreslot.Grant, error) {
	c, err := coreslot.Open(o)
	if err != nil {
		return nil, err
	}
	defer c.Close() // the grant outlives its Client
	w, err := c.Enqueue(ctx, coreslot.Request{Label: "example"})
	if err != nil {
		return nil, err
	}
	return w.Wait(ctx)
}
