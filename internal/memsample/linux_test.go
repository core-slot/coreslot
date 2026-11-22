//go:build linux

package memsample

import (
	"os"
	"slices"
	"testing"
	"time"
)

// A name may hold spaces and either parenthesis, so the fields start after the last ')'. Mutation: the
// first ')' for the last → "a) (b" is cut short and its fields shift, red.
func TestParseStat(t *testing.T) {
	name, f, ok := parseStat("42 (a) (b c) S 7 42 42\n")
	if !ok || name != "a) (b c" || !slices.Equal(f, []string{"S", "7", "42", "42"}) {
		t.Errorf("parseStat = %q, %q, %v", name, f, ok)
	}
	if _, _, ok := parseStat("42 no name"); ok {
		t.Errorf("parseStat(no parenthesis) ok, want not")
	}
}

// The load, the cpu line and, where the kernel reports it, PSI cpu read live, and a second of
// ticks passes. Mutations: a wrong path for /proc/stat leaves CPUTotal 0; the PSI cpu read left out
// keeps HasPSICPU false on a kernel with the file.
func TestReadHostLinuxCPULive(t *testing.T) {
	a, err := ReadHost()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	b, err := ReadHost()
	if err != nil {
		t.Fatal(err)
	}
	if !a.HasLoad || a.CPUTotal == 0 || b.CPUTotal <= a.CPUTotal || b.CPUBusy < a.CPUBusy || b.CPUBusy > b.CPUTotal {
		t.Errorf("CPU: load %v (%v), ticks %d/%d then %d/%d; want a load and ticks that advance", a.HasLoad, a.Load1,
			a.CPUBusy, a.CPUTotal, b.CPUBusy, b.CPUTotal)
	}
	if _, err := os.Stat(procPSICPU); err == nil && hasPSICPU() != b.HasPSICPU {
		t.Errorf("PSI cpu: the kernel reports it (%v), the reading has it %v", hasPSICPU(), b.HasPSICPU)
	}
}
