//go:build darwin || linux

package memsample

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// helperEnv makes the test binary its own helper: it allocates that many MiB, fills them with
// incompressible bytes, prints "ready" and holds them for a minute.
const helperEnv = "MEMSAMPLE_HELPER_MIB"

func TestMain(m *testing.M) {
	if v := os.Getenv(helperEnv); v != "" {
		mib, err := strconv.Atoi(v)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		b := make([]byte, mib<<20)
		_, _ = rand.NewChaCha8([32]byte{1}).Read(b)
		fmt.Println("ready")
		time.Sleep(time.Minute)
		runtime.KeepAlive(b)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A shell that started two helpers of 300 MiB each holds about 600 MiB as a tree, though the
// shell itself holds almost nothing. Mutation: sum the root alone, and the tree is a few MiB.
func TestTreeCountsChildren(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", `"$0" & "$0" & wait`, self)
	cmd.Env = append(os.Environ(), helperEnv+"=300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	ready := make(chan int)
	go func() {
		n := 0
		for sc := bufio.NewScanner(out); sc.Scan(); {
			if sc.Text() == "ready" {
				n++
				ready <- n
			}
		}
		close(ready)
	}()
	for n := 0; n < 2; {
		select {
		case v, ok := <-ready:
			if !ok {
				t.Fatalf("the helpers ended after %d ready lines", n)
			}
			n = v
		case <-time.After(30 * time.Second):
			t.Fatalf("%d of 2 helpers ready after 30 s", n)
		}
	}
	kib, ok := Tree(cmd.Process.Pid)
	if mib := kib / 1024; !ok || mib < 540 || mib > 760 {
		t.Errorf("Tree = %d MiB, %v; want 540..760 MiB, true", mib, ok)
	}
}

// A process that has exited and been reaped is not there to read. Mutation: report ok for an
// empty sum in treeSum.
func TestTreeOfExitedPID(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if kib, ok := Tree(cmd.Process.Pid); ok {
		t.Errorf("Tree(exited) = %d, true; want false", kib)
	}
}

func TestHeadroom(t *testing.T) {
	total, err := Total()
	if err != nil {
		t.Fatal(err)
	}
	if mib, ok := Headroom(); !ok || mib <= 0 || mib > total {
		t.Errorf("Headroom = %d, %v; want 1..%d, true", mib, ok, total)
	}
}

func BenchmarkTree(b *testing.B) {
	for b.Loop() {
		Tree(os.Getpid())
	}
}

// Mutation: build StartNS from the seconds alone (no ×1e9) on macOS, or without btime on Linux,
// and the start is decades before the last hour.
func TestListingHasSelf(t *testing.T) {
	procs, ok := Listing()
	if !ok {
		t.Fatal("Listing() not ok")
	}
	p, ok := procs[os.Getpid()]
	now := time.Now()
	if !ok || p.PPID != os.Getppid() {
		t.Fatalf("Listing()[%d] = %+v, %v; want PPID %d", os.Getpid(), p, ok, os.Getppid())
	}
	if start := time.Unix(0, p.StartNS); start.Before(now.Add(-time.Hour)) || start.After(now) {
		t.Errorf("StartNS = %v; want within the last hour before %v", start, now)
	}
}

// The cost the admission section pays per live attempt: one listing of the machine, and the use
// of the test's own tree in a table of 500 processes.
func BenchmarkListingHolderUse(b *testing.B) {
	listed, ok := Listing()
	if !ok {
		b.Fatal("Listing() not ok")
	}
	self := os.Getpid()
	procs := map[int]Proc{self: listed[self]}
	for pid := 1 << 22; len(procs) < 500; pid++ {
		procs[pid] = Proc{PPID: 1}
	}
	roots := map[int]int64{self: time.Now().UnixNano()}
	for b.Loop() {
		if _, ok := Listing(); !ok {
			b.Fatal("Listing() not ok")
		}
		if use := HolderUse(procs, roots); len(use) != 1 {
			b.Fatalf("HolderUse = %v; want one root", use)
		}
	}
}
