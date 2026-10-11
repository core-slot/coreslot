//go:build darwin

package config

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestMachineCPUs(t *testing.T) {
	want, err := unix.SysctlUint32("hw.logicalcpu")
	if err != nil {
		t.Fatal(err)
	}
	got, err := MachineCPUs()
	if err != nil || got != int(want) {
		t.Fatalf("MachineCPUs() = %d, %v; want hw.logicalcpu = %d", got, err, want)
	}
}
