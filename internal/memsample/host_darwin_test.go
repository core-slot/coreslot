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
	if b.Decomp < a.Decomp || b.SwapIns < a.SwapIns || b.CompressBytes < a.CompressBytes {
		t.Errorf("counters went back: %+v then %+v", a, b)
	}
	if span := time.Duration(b.WallNS - a.WallNS); span < time.Second || span > 3*time.Second {
		t.Errorf("WallNS advanced %v over a 1 s sleep", span)
	}
}
