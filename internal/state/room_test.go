package state

import "testing"

// TestRoom pins the room's arithmetic. Mutations: clamping max(0, Σneed − Σfootprint) over all
// holds instead of per hold reddens "one hold over, one under its need"; dropping the max(0, …)
// per hold reddens "hold past its need".
func TestRoom(t *testing.T) {
	cases := []struct {
		name        string
		headroomMiB int64
		holds       []MemHold
		room        int64
	}{
		{"no holds", 12000, nil, 12000},
		{"unmeasured hold counts whole", 12000, []MemHold{{3200, 0}}, 8800},
		{"half-used hold", 12000, []MemHold{{3200, 1600}}, 10400},
		{"hold past its need", 12000, []MemHold{{3200, 4000}}, 12000},
		{"one hold over, one under its need", 12000, []MemHold{{3200, 4000}, {3200, 0}}, 8800},
		{"negative", 1000, []MemHold{{3200, 0}}, -2200},
	}
	for _, c := range cases {
		if got := Room(c.headroomMiB, c.holds); got != c.room {
			t.Errorf("%s: Room(%d, %v) = %d; want %d", c.name, c.headroomMiB, c.holds, got, c.room)
		}
	}
}
