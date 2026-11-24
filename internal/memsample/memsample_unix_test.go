//go:build darwin || linux

package memsample

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
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

// twoHelpers starts a shell that starts two helpers of 300 MiB each, and returns it once both are
// ready; it kills them when the test ends.
func twoHelpers(t testing.TB) *exec.Cmd {
	t.Helper()
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
	return cmd
}

// A shell that started two helpers of 300 MiB each holds about 600 MiB as a tree, though the
// shell itself holds almost nothing. Mutation: sum the root alone, and the tree is a few MiB.
func TestTreeCountsChildren(t *testing.T) {
	cmd := twoHelpers(t)
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
	if mib, ok := Headroom(); !ok || mib <= 0 || mib > total {
		t.Errorf("Headroom = %d, %v; want 1..%d, true", mib, ok, total)
	}
}

// selfRoot is the test process as a root to measure, with its listed start time.
func selfRoot(t testing.TB) Root {
	t.Helper()
	procs, ok := Listing()
	if !ok {
		t.Fatal("Listing() not ok")
	}
	return Root{PID: os.Getpid(), StartNS: procs[os.Getpid()].StartNS}
}

// The test process's descendants, a shell and its two helpers of 300 MiB, sum to about 600 MiB,
// and the test process's own footprint is left out. Mutations: walk the direct children alone (the
// shell, a few MiB); count the root too (equal to Tree of the test process).
func TestFootprintsSumsDescendants(t *testing.T) {
	twoHelpers(t)
	root := selfRoot(t)
	own, _, ok := footprint(root.PID)
	if !ok {
		t.Fatal("the test process's own footprint is unreadable")
	}
	got, ok := Footprints([]Root{root})
	treeKiB, _, treeOK := Tree(root.PID)
	if !ok || !treeOK {
		t.Fatalf("Footprints ok %v, Tree ok %v", ok, treeOK)
	}
	if mib := got[root.PID]; mib < 540 || mib > 760 || mib > treeKiB>>10-own>>11 {
		t.Errorf("Footprints = %d MiB; want 540..760 MiB and below Tree's %d MiB by about the process's own %d MiB",
			mib, treeKiB>>10, own>>10)
	}
}

// A live root with no child has an entry of 0. Mutation: give no entry to a root with nothing read.
func TestFootprintsOfAChildlessRoot(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	procs, ok := Listing()
	if !ok {
		t.Fatal("Listing() not ok")
	}
	pid := cmd.Process.Pid
	got, ok := Footprints([]Root{{PID: pid, StartNS: procs[pid].StartNS}})
	if mib, has := got[pid]; !ok || !has || mib != 0 {
		t.Errorf("Footprints = %v, %v; want {%d: 0}, true", got, ok, pid)
	}
}

// A reaped pid, a pid listed with a later start than its hold's (reused), and pids 0 and 1 have
// no entry. Mutations: report 0 for a root that is not listed; drop the start-time test; walk
// from pid 1.
func TestFootprintsSkipsRootsItCannotTrust(t *testing.T) {
	done := exec.Command("true")
	if err := done.Run(); err != nil {
		t.Fatal(err)
	}
	self := selfRoot(t)
	reused := Root{PID: self.PID, StartNS: self.StartNS - int64(10*time.Second)}
	procs, _ := Listing() // pids 0 and 1 with their own start times, so that only the pid test refuses them
	for _, r := range []Root{{PID: done.Process.Pid}, reused, {PID: 0, StartNS: procs[0].StartNS}, {PID: 1, StartNS: procs[1].StartNS}} {
		if got, ok := Footprints([]Root{r}); !ok || len(got) != 0 {
			t.Errorf("Footprints(%+v) = %v, %v; want no entry, true", r, got, ok)
		}
	}
}

// Three roots: the test process and two childless children.
func BenchmarkFootprints(b *testing.B) {
	roots := []Root{selfRoot(b)}
	for range 2 {
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		procs, _ := Listing()
		roots = append(roots, Root{PID: cmd.Process.Pid, StartNS: procs[cmd.Process.Pid].StartNS})
	}
	for b.Loop() {
		Footprints(roots)
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

// A process named docker is a container client, one named dockerd is not, and the walk goes
// below the root. The binary is a copy of the test binary, in helper mode: a copy of sleep is
// killed at exec on a current macOS, and a symlink or a script reports the name of its target or
// its interpreter. Mutation: test the root's name alone, and the sh case reddens; make
// ContainerProgram accept dockerd, and the dockerd cases redden.
func TestContainerClientName(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	// Each name is written once and the shell cases run the same files again: on Linux a
	// race-built helper's binary stays busy for some milliseconds after Wait has reaped it, and
	// writing it again then fails with "text file busy".
	dir := t.TempDir()
	for _, name := range []string{"docker", "dockerd"} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name  string
		shell bool
		want  bool
	}{{"docker", false, true}, {"dockerd", false, false}, {"docker", true, true}, {"dockerd", true, false}} {
		bin := filepath.Join(dir, c.name)
		cmd := exec.Command(bin)
		if c.shell {
			cmd = exec.Command("/bin/sh", "-c", `"$0" & wait`, bin)
		}
		cmd.Env = append(os.Environ(), helperEnv+"=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatalf("helper %s said %q, %v; want ready", c.name, line, err)
		}
		if got := ContainerClient(cmd.Process.Pid); got != c.want {
			t.Errorf("ContainerClient(%s, shell %v) = %v, want %v", c.name, c.shell, got, c.want)
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}
}
