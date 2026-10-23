package state

import "testing"

// TestLiveMemSize pins the live size's arithmetic. Mutations: dropping the unlisted tokens
// reddens "unlisted tokens count whole"; clamping max(0, Σreserved − Σused) over all holds
// instead of per hold reddens "one hold over, one under its reservation" (it gives 125); omitting
// the max(static, …) floor reddens "room negative: the floor".
func TestLiveMemSize(t *testing.T) {
	cases := []struct {
		name           string
		static, locked int
		headroomMiB    int64
		holds          []MemHold
		size           int
		raised         bool
	}{
		{"unused hold counts whole", 76, 25, 12000, []MemHold{{25, -1}}, 93, true},
		{"half-used hold", 76, 25, 12000, []MemHold{{25, 2252800}}, 110, true},
		{"hold past its reservation", 76, 25, 12000, []MemHold{{25, 4096000}}, 118, true},
		{"unlisted tokens count whole", 76, 30, 12000, []MemHold{{25, -1}}, 93, true},
		{"room negative: the floor", 76, 25, 1000, []MemHold{{25, -1}}, 76, false},
		{"locked above static, no room", 76, 90, 0, []MemHold{{90, -1}}, 90, true},
		{"cap", 76, 0, 1000000, nil, 4096, true},
		{"one hold over, one under its reservation", 76, 50, 12000, []MemHold{{25, 4096000}, {25, -1}}, 118, true},
	}
	for _, c := range cases {
		size, raised := LiveMemSize(c.static, c.locked, c.headroomMiB, c.holds)
		if size != c.size || raised != c.raised {
			t.Errorf("%s: LiveMemSize(%d, %d, %d, %v) = %d, %v; want %d, %v",
				c.name, c.static, c.locked, c.headroomMiB, c.holds, size, raised, c.size, c.raised)
		}
	}
}
