//go:build unix

package coreslot_test

import (
	"context"
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

// Queue for one CPU core, wait for it, and run a command inside the grant: the command inherits
// the grant's descriptors and learns what it may use from CORESLOT_GRANTED_CPU.
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
		Need: map[string]coreslot.Range{"cpu": {Min: 1, Max: 1}}})
	if err != nil {
		log.Fatal(err)
	}
	g, err := w.Wait(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer g.Close()

	cmd := exec.CommandContext(ctx, "sh", "-c", `echo "the command may use $CORESLOT_GRANTED_CPU core"`)
	cmd.ExtraFiles, cmd.Env = g.Files(), g.Environ(os.Environ())
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		log.Fatal(err)
	}
	// Output: the command may use 1 core
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
	w, err := c.Enqueue(context.Background(), coreslot.Request{Label: "example"}) // cpu 1..1
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
			fmt.Println("admitted with cpu", g.Granted()["cpu"])
			g.Close()
			return
		}
		<-tick.C // or do other work in between
	}
	// Output: admitted with cpu 1
}

// Adopt work that is already running: it takes what is free, without queueing, and reports
// what the work runs without.
func ExampleClient_Adopt() {
	dir, err := os.MkdirTemp("", "coreslot-example")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	// A budget of two cores, whatever the machine has.
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[resources]\ncpu = 2\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	c, err := coreslot.Open(coreslot.Options{StateDir: dir})
	if err != nil {
		log.Fatal(err)
	}
	g, err := c.Adopt(context.Background(), coreslot.Request{Label: "already running",
		Need: map[string]coreslot.Range{"cpu": {Min: 3, Max: 4}}})
	if err != nil {
		log.Fatal(err)
	}
	defer g.Close()
	fmt.Printf("granted %d, short of the minimum by %d\n", g.Granted()["cpu"], g.Shortfall()["cpu"])
	// Output: granted 2, short of the minimum by 1
}

// Read the budget, its holds and its queue, as `coreslot status` does.
func ExampleClient_Snapshot() {
	dir, err := os.MkdirTemp("", "coreslot-example")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[resources]\ncpu = 4\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	c, err := coreslot.Open(coreslot.Options{StateDir: dir})
	if err != nil {
		log.Fatal(err)
	}
	g, err := c.Adopt(context.Background(), coreslot.Request{Label: "build",
		Need: map[string]coreslot.Range{"cpu": {Min: 3, Max: 3}}})
	if err != nil {
		log.Fatal(err)
	}
	defer g.Close()

	r, err := c.Snapshot()
	if err != nil {
		log.Fatal(err)
	}
	cpu := r.Resources["cpu"]
	fmt.Printf("cpu: size %d, locked %d, available %d\n", cpu.Size, cpu.Locked, cpu.Available)
	for _, h := range r.Holds {
		fmt.Printf("%s holds cpu %d (adopted %v)\n", h.Label, h.Granted["cpu"], h.Adopted)
	}
	fmt.Println("queued:", len(r.Tickets))
	// Output:
	// cpu: size 4, locked 3, available 1
	// build holds cpu 3 (adopted true)
	// queued: 0
}
