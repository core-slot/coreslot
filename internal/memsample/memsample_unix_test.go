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

// spinEnv makes the test binary a helper that spins until it has spent 1 s in user code, prints
// "ready" and sleeps for a minute.
const spinEnv = "MEMSAMPLE_HELPER_SPIN"

func TestMain(m *testing.M) {
	if os.Getenv(spinEnv) != "" {
		for n := 0; ; n++ {
			var ru syscall.Rusage
			if n%1_000_000 == 0 {
				if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil || ru.Utime.Sec >= 1 {
					break
				}
			}
		}
		fmt.Println("ready")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
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
	kib, _, ok := Tree(cmd.Process.Pid)
	if mib := kib / 1024; !ok || mib < 540 || mib > 760 {
		t.Errorf("Tree = %d MiB, %v; want 540..760 MiB, true", mib, ok)
	}
}

// A child that spent 1 s in user code reads as about 1e9 ns of user time. Mutation: leave out
// the mach tick conversion on Apple Silicon, and it reads as 24 ms.
func TestTreeTimesTicks(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), spinEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper said %q, %v; want ready", line, err)
	}
	_, times, ok := Tree(cmd.Process.Pid)
	if tt := times[cmd.Process.Pid]; !ok || tt.UserNS < 0.8e9 || tt.UserNS > 1.2e9 {
		t.Errorf("Tree's times = %+v, %v; want UserNS within 20 %% of 1e9", tt, ok)
	}
}

// A process that has exited and been reaped is not there to read. Mutation: report ok for an
// empty sum in treeSum.
func TestTreeOfExitedPID(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if kib, _, ok := Tree(cmd.Process.Pid); ok {
		t.Errorf("Tree(exited) = %d, true; want false", kib)
	}
}

func TestHeadroom(t *testing.T) {
	total, err := Total()
	if err != nil {
		t.Fatal(err)
	}
	// The strict macOS reading may be small: the file cache the kernel keeps can exceed what is
	// free. macOS reads the parts and returns their strict reading; Linux reads none.
	mib, parts, ok := Headroom()
	if !ok || mib < 0 || mib > total {
		t.Errorf("Headroom = %d, %v; want 0..%d, true", mib, ok, total)
	}
	switch {
	case runtime.GOOS == "linux" && parts != nil:
		t.Errorf("Linux Headroom has parts %+v; want none", *parts)
	case runtime.GOOS == "darwin" && parts == nil:
		t.Error("macOS Headroom has no parts")
	case parts != nil:
		if strict, _ := parts.Headroom(0, nil, true); strict != mib || parts.InternalMiB <= 0 {
			t.Errorf("Headroom = %d, parts %+v; want their strict reading %d and the anonymous memory read", mib, *parts, strict)
		}
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
