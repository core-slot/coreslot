package memsample

import (
	"testing"
	"time"
)

// Mutation: swap the two clocks, and a machine that has slept since boot reads awake longer than
// wall; read the clocks in µs, and the second read is not 1 s later.
func TestReadHostDarwinLive(t *testing.T) {
	a, err := ReadHost()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	b, err := ReadHost()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []HostReading{a, b} {
		if r.WallNS < r.AwakeNS || r.VMVolumeFreeMiB <= 0 || r.SwapUsedMiB < 0 || r.SwapFreeMiB < 0 {
			t.Errorf("ReadHost = %+v; want WallNS >= AwakeNS, VMVolumeFreeMiB > 0, swap >= 0", r)
		}
	}
	if b.Decomp < a.Decomp || b.SwapIns < a.SwapIns {
		t.Errorf("counters went back: %+v then %+v", a, b)
	}
	// The load and HOST_CPU_LOAD_INFO read, and a second of ticks passes on every core at 100 Hz.
	// Mutation: a wrong flavor fails the call and leaves CPUTotal 0.
	if !a.HasLoad || a.CPUTotal == 0 || b.CPUTotal <= a.CPUTotal || b.CPUBusy < a.CPUBusy || b.CPUBusy > b.CPUTotal {
		t.Errorf("CPU: load %v (%v), ticks %d/%d then %d/%d; want a load and ticks that advance", a.HasLoad, a.Load1,
			a.CPUBusy, a.CPUTotal, b.CPUBusy, b.CPUTotal)
	}
	if span := time.Duration(b.WallNS - a.WallNS); span < time.Second || span > 3*time.Second {
		t.Errorf("WallNS advanced %v over a 1 s sleep", span)
	}
}

// HOST_CPU_LOAD_INFO's slots are user, system, idle and nice; only idle is not busy. Mutation:
// counting slot 2 as busy, or leaving nice (slot 3) out, gives another busy sum.
func TestBusyTicksSlotOrder(t *testing.T) {
	if busy, total := busyTicks([4]uint32{1, 10, 100, 1000}); busy != 1011 || total != 1111 {
		t.Errorf("busyTicks = %d of %d; want 1011 of 1111", busy, total)
	}
}
