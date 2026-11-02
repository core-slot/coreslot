package memsample

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMeminfo(t *testing.T) {
	const text = "MemTotal:       16303412 kB\nMemAvailable: 9000000 kB\n"
	for key, want := range map[string]int64{"MemTotal": 16303412, "MemAvailable": 9000000} {
		if got, ok := meminfo(text, key); !ok || got != want {
			t.Errorf("meminfo(%s) = %d, %v; want %d", key, got, ok, want)
		}
	}
	if got, ok := meminfo(text, "Foo"); ok {
		t.Errorf("meminfo(Foo) = %d, true; want not found", got)
	}
}

// Mutation: stop the walk at the root's children (no grandchildren), and 6 is missing.
func TestDescendants(t *testing.T) {
	parents := map[int]int{2: 1, 3: 2, 4: 2, 5: 1, 6: 3}
	if got := fmt.Sprint(descendants(2, parents)); got != "[2 3 4 6]" {
		t.Errorf("descendants(2) = %s, want [2 3 4 6]", got)
	}
}

// A process that exited after the listing cannot be read: it is skipped, and the rest still
// add up. Mutation: return !ok on the first failed read.
func TestTreeSumSkipsFailedReads(t *testing.T) {
	read := func(pid int) (int64, TreeTimes, bool) {
		switch pid {
		case 2:
			return 100, TreeTimes{UserNS: 1}, true
		case 4:
			return 50, TreeTimes{UserNS: 2}, true
		}
		return 0, TreeTimes{}, false
	}
	if kib, times, ok := treeSum(2, map[int]int{3: 2, 4: 2}, read); kib != 150 || len(times) != 2 ||
		times[2].UserNS != 1 || times[4].UserNS != 2 || !ok {
		t.Errorf("treeSum = %d, %v, %v; want 150, the times of 2 and 4, true", kib, times, ok)
	}
}

// A root that is gone and lists no descendants reads nothing: not a measured zero. Mutation:
// return true for an empty sum.
func TestTreeSumRootGone(t *testing.T) {
	read := func(int) (int64, TreeTimes, bool) { return 0, TreeTimes{}, false }
	if kib, _, ok := treeSum(7, map[int]int{3: 2}, read); kib != 0 || ok {
		t.Errorf("treeSum = %d, %v; want 0, false", kib, ok)
	}
}

// Mutation: read the leaf only (no walk up), and a leaf of max under a 4 GiB parent gives no
// limit instead of 3072.
func TestCgroupLimit(t *testing.T) {
	const gib = 1 << 30
	files := func(kv ...any) func(string) (string, error) {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = fmt.Sprint(kv[i+1]) + "\n"
		}
		return func(path string) (string, error) {
			if v, ok := m[path]; ok {
				return v, nil
			}
			return "", os.ErrNotExist
		}
	}
	const self = "0::/a/b\n"
	for _, c := range []struct {
		name   string
		self   string
		read   func(string) (string, error)
		mib    int64
		wantOK bool
	}{
		{"leaf max, parent 4 GiB with 1 GiB used", self, files(
			"/sys/fs/cgroup/a/b/memory.max", "max", "/sys/fs/cgroup/a/b/memory.current", gib,
			"/sys/fs/cgroup/a/memory.max", 4*gib, "/sys/fs/cgroup/a/memory.current", gib), 3072, true},
		{"leaf 2 GiB with 1.5 GiB used under 4 GiB", self, files(
			"/sys/fs/cgroup/a/b/memory.max", 2*gib, "/sys/fs/cgroup/a/b/memory.current", 3*gib/2,
			"/sys/fs/cgroup/a/memory.max", 4*gib, "/sys/fs/cgroup/a/memory.current", 2*gib), 512, true},
		{"every level max", self, files(
			"/sys/fs/cgroup/a/b/memory.max", "max", "/sys/fs/cgroup/a/b/memory.current", gib,
			"/sys/fs/cgroup/a/memory.max", "max", "/sys/fs/cgroup/a/memory.current", gib), 0, false},
		{"v1 only", "12:memory:/a/b\n1:name=systemd:/a/b\n", files(
			"/sys/fs/cgroup/a/b/memory.max", gib, "/sys/fs/cgroup/a/b/memory.current", 0), 0, false},
		{"current above max", self, files(
			"/sys/fs/cgroup/a/b/memory.max", gib, "/sys/fs/cgroup/a/b/memory.current", 2*gib), 0, true},
	} {
		if mib, ok := cgroupLimit(c.self, c.read); mib != c.mib || ok != c.wantOK {
			t.Errorf("%s: cgroupLimit = %d, %v; want %d, %v", c.name, mib, ok, c.mib, c.wantOK)
		}
	}
}

// A laptop that slept for ten minutes between two samples advanced its sleep-inclusive clock by
// 600 s and its awake clock by 2 s: a gap. Mutation: drop the divergence check, and a 3 s sleep
// inside one period passes.
func TestClockPairDetectsSleep(t *testing.T) {
	const s = int64(time.Second)
	prev := HostReading{WallNS: 1000 * s, AwakeNS: 900 * s}
	for _, c := range []struct {
		name        string
		wall, awake int64
		want        bool
	}{
		{"slept 598 s", 600 * s, 2 * s, true},
		{"slept 3 s", 5 * s, 2 * s, true},
		{"one period awake", 2 * s, 2 * s, false},
		{"sampler stalled 7 s", 7 * s, 7 * s, true},
		{"a reading from before a reboot", -990 * s, -990 * s, true},
	} {
		cur := HostReading{WallNS: prev.WallNS + c.wall, AwakeNS: prev.AwakeNS + c.awake}
		if got := Gap(prev, cur); got != c.want {
			t.Errorf("%s: Gap = %v, want %v", c.name, got, c.want)
		}
	}
}

// The counters count from boot, so one that went back is a reboot and a gap even one period later;
// the stall counts only when both readings have it. Mutations: dropping any counter's term makes
// its row comparable; comparing the stall without both readings having it makes the last row a gap.
func TestCounterBackIsAGap(t *testing.T) {
	const s = int64(time.Second)
	prev := HostReading{Decomp: 100, SwapIns: 100, PSISomeUS: 100, HasPSI: true, WallNS: 1000 * s, AwakeNS: 900 * s}
	for _, c := range []struct {
		name string
		edit func(*HostReading)
		want bool
	}{
		{"all forward", func(*HostReading) {}, false},
		{"decompressions back", func(r *HostReading) { r.Decomp = 1 }, true},
		{"swap-ins back", func(r *HostReading) { r.SwapIns = 1 }, true},
		{"stall back", func(r *HostReading) { r.PSISomeUS = 1 }, true},
		{"stall missing", func(r *HostReading) { r.PSISomeUS, r.HasPSI = 0, false }, false},
	} {
		cur := prev
		cur.Decomp, cur.SwapIns, cur.PSISomeUS = 200, 200, 200
		cur.WallNS, cur.AwakeNS = prev.WallNS+2*s, prev.AwakeNS+2*s
		c.edit(&cur)
		if got := Gap(prev, cur); got != c.want {
			t.Errorf("%s: Gap = %v, want %v", c.name, got, c.want)
		}
	}
}

// Mutation: take the total of the "full" line, and the stall reads 678.
func TestPSITotalParse(t *testing.T) {
	const text = "some avg10=0.00 avg60=0.12 avg300=0.05 total=12345\n" +
		"full avg10=0.00 avg60=0.00 avg300=0.00 total=678\n"
	if us, ok := psiTotal(text); !ok || us != 12345 {
		t.Errorf("psiTotal = %d, %v; want 12345, true", us, ok)
	}
	for _, bad := range []string{"", "full avg10=0.00 total=678\n", "some avg10=0.00 total=x\n"} {
		if us, ok := psiTotal(bad); ok {
			t.Errorf("psiTotal(%q) = %d, true; want not found", bad, us)
		}
	}
}

// Mutation: match the key as a prefix, and pswpin_zero's 9 is read.
func TestVmstatParse(t *testing.T) {
	const text = "nr_free_pages 1000\npswpin_zero 9\npswpin 42\npswpout 7\n"
	if n, ok := vmstat(text, "pswpin"); !ok || n != 42 {
		t.Errorf("vmstat(pswpin) = %d, %v; want 42, true", n, ok)
	}
	if n, ok := vmstat(text, "pgmajfault"); ok {
		t.Errorf("vmstat(pgmajfault) = %d, true; want not found", n)
	}
}

// The watchdog reads the machine inside its tick, where a subprocess would cost more than the
// tick. Mutation: import os/exec in any file of the package.
func TestNoSubprocessImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("Glob = %v, %v", files, err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if imp.Path.Value == `"os/exec"` {
				t.Errorf("%s imports os/exec", name)
			}
		}
	}
}

// The readings hook names HostReading's fields: it replaces those it names and keeps the others.
func TestReadingsReplaceNamedFields(t *testing.T) {
	r := overlay(HostReading{SwapIns: 7, SwapUsedMiB: 9}, map[string]int64{"Decomp": 5,
		"HasPSI": 1, "SwapFreeMiB": 100, "WallNS": 3e9, "AwakeNS": 2e9})
	want := HostReading{Decomp: 5, SwapIns: 7, HasPSI: true, SwapUsedMiB: 9, SwapFreeMiB: 100, WallNS: 3e9, AwakeNS: 2e9}
	if r != want {
		t.Errorf("overlay = %+v, want %+v", r, want)
	}
}
