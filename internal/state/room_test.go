package state

import "testing"

// TestRoomSubtractsRemainingGrowth: the headroom less the reserve and each hold's max(0, need −
// footprint). Mutations: clamping max(0, Σneed − Σfootprint) over all holds instead of per hold
// reddens "one hold over, one under its need"; dropping the max(0, …) per hold reddens "hold past
// its need"; forgetting the reserve reddens every row.
func TestRoomSubtractsRemainingGrowth(t *testing.T) {
	cases := []struct {
		name        string
		headroomMiB int64
		holds       []MemHold
		room        int64
	}{
		{"no holds", 12000, nil, 10976},
		{"one hold", 8192, []MemHold{{2048, 512}}, 5632},
		{"unmeasured hold counts whole", 12000, []MemHold{{3200, 0}}, 7776},
		{"hold past its need", 12000, []MemHold{{3200, 4000}}, 10976},
		{"one hold over, one under its need", 12000, []MemHold{{3200, 4000}, {3200, 0}}, 7776},
	}
	for _, c := range cases {
		if got := Room(c.headroomMiB, 1024, c.holds); got != c.room {
			t.Errorf("%s: Room(%d, 1024, %v) = %d; want %d", c.name, c.headroomMiB, c.holds, got, c.room)
		}
	}
}

// TestRoomCanBeNegative: a room below zero is not clamped, so that a caller sees by how much a
// need misses it.
func TestRoomCanBeNegative(t *testing.T) {
	if got := Room(1000, 1024, []MemHold{{3200, 0}}); got != -3224 {
		t.Errorf("Room = %d; want -3224", got)
	}
}
