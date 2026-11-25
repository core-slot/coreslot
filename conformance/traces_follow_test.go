//go:build coreslot_testhooks && unix

package conformance

import (
	"testing"

	"github.com/core-slot/coreslot/internal/store"
)

// A learned run whose command uses far less than its need follows what it uses once its label's
// window has passed, and a waiter that does not fit beside its whole need starts; while both run,
// reserved_mib is what the holds reserve and the room subtracts exactly that less what each
// uses. The readings hook lowers the window's floor to 1 s, and three lines of the label carrying a
// window of 1 s make it 2 s. The headroom is of a machine's scale, so that the bound (the machine's
// memory less the reserve) holds big's need. Mutation: no run follows (follows false) → the
// waiter never starts, red.
func TestTraceFollowAdmitsWaiter(t *testing.T) {
	e := State(t)
	e.headroomMiB = 7000
	e.WriteConfig("[memory]\nreserve = \"1000M\"\n") // a room of 6000: 4 GiB and 3 GiB do not fit together
	readings := calm(0)
	readings["TailFloorS"], readings["TailDefaultS"] = 1, 2
	e.SetReadings(readings)
	peakKiB, windowS := int64(4<<20), 1.0
	for range 3 {
		rec := store.RunRecord{TimeNS: store.NowNS(), Label: "big", MemPeakKiB: &peakKiB, MemWindowS: &windowS}
		if err := e.Engine().Store().AppendRun(rec); err != nil {
			t.Fatal(err)
		}
	}

	big := e.Run("big")
	e.WaitStarted("big")
	waiter := e.Run("waiter", "--mem", "3G")
	e.WaitStarted("waiter")

	d := e.statusDoc()
	b, w := d.hold(t, "big"), d.hold(t, "waiter")
	if b.AskedMiB != 4096 || b.NeedMiB >= 4096 || b.Trimmed {
		t.Errorf("big: %+v; want asked_mib 4096, need_mib below it, not trimmed", b)
	}
	if d.Mem.ReservedMiB != b.NeedMiB+w.NeedMiB || d.Mem.RoomMiB != 6000-b.growth()-w.growth() {
		t.Errorf("reserved_mib %d, room_mib %d; want %d and %d (big %+v, waiter %+v)", d.Mem.ReservedMiB,
			d.Mem.RoomMiB, b.NeedMiB+w.NeedMiB, 6000-b.growth()-w.growth(), b, w)
	}

	e.ReleaseChild("waiter")
	waiter.ExitCode(0)
	e.ReleaseChild("big")
	big.ExitCode(0)
}
