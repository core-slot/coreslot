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
		{"one hold", 8192, []MemHold{{2048, 512, 0}}, 5632},
		{"unmeasured hold counts whole", 12000, []MemHold{{3200, 0, 0}}, 7776},
		{"hold past its need", 12000, []MemHold{{3200, 4000, 0}}, 10976},
		{"one hold over, one under its need", 12000, []MemHold{{3200, 4000, 0}, {3200, 0, 0}}, 7776},
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
	if got := Room(1000, 1024, []MemHold{{3200, 0, 0}}); got != -3224 {
		t.Errorf("Room = %d; want -3224", got)
	}
}

// TestRoomSubtractsGrowth: every room subtracts each live hold's reserved VM growth on top of its
// remaining need, a hold that reserves only growth included; a gated container request's room
// subtracts the growth alone. 8192 − 1024 − (1536 + 1000) − (0 + 500) = 4132, and 8192 − 1024 −
// 1500 = 5668. Mutations: dropping Σ GrowthMiB from Room gives 5632; dropping it from GrowthRoom
// gives 7168.
func TestRoomSubtractsGrowth(t *testing.T) {
	holds := []MemHold{{NeedMiB: 2048, FootprintMiB: 512, GrowthMiB: 1000}, {GrowthMiB: 500}}
	if got := Room(8192, 1024, holds); got != 4132 {
		t.Errorf("Room = %d; want 4132", got)
	}
	if got := GrowthRoom(8192, 1024, holds); got != 5668 {
		t.Errorf("GrowthRoom = %d; want 5668", got)
	}
}

// TestGrowth: min(max(0, V − vm_now), k × max(0, need − footprint)) rounded up, k 2 on the Apple
// backend and 3/2 on Docker VMM. Each row names the mutation it catches.
func TestGrowth(t *testing.T) {
	cases := []struct {
		name               string
		v, vmNow, need, fp int64
		vmm                bool
		want               int64
	}{
		// Dropping the max(0, …) around V − vm_now gives −1000.
		{"VM past its size", 8000, 9000, 4000, 0, false, 0},
		{"VM at its size, VMM", 8000, 8000, 4000, 0, true, 0},
		// Dropping the max(0, …) around need − footprint gives −2000.
		{"need overfilled", 26005, 10000, 4000, 5000, false, 0},
		// Dropping the first term gives 8000.
		{"capped by the VM", 26005, 25005, 4000, 0, false, 1000},
		// k = 1 on the Apple backend gives 2000.
		{"Apple: twice the remaining need", 26005, 6005, 3000, 1000, false, 4000},
		// k = 2 on VMM, the backend ignored, gives 4000.
		{"VMM: 3/2 the remaining need", 26005, 6005, 3000, 1000, true, 3000},
		// Rounding down gives 1501.
		{"VMM: rounded up", 26005, 6005, 2001, 1000, true, 1502},
	}
	for _, c := range cases {
		if got := Growth(c.v, c.vmNow, c.need, c.fp, c.vmm); got != c.want {
			t.Errorf("%s: Growth(V %d, vm_now %d, need %d, footprint %d, vmm %v) = %d; want %d", c.name, c.v, c.vmNow, c.need, c.fp, c.vmm, got, c.want)
		}
	}
}

// TestDockerRoomFormula: free_est less the Docker reserve and each hold's max(0, docker_need −
// docker_footprint): 20000 − 3072 − 3000 − 0 − 1000 = 12928. Mutation: dropping the max(0, …)
// counts the overfilled hold as −1000 and gives 13928.
func TestDockerRoomFormula(t *testing.T) {
	holds := []DockerHold{{NeedMiB: 4000, FootprintMiB: 1000}, {NeedMiB: 2000, FootprintMiB: 3000}, {NeedMiB: 1000}}
	if got := DockerRoom(20000, 3072, holds); got != 12928 {
		t.Errorf("DockerRoom = %d; want 12928", got)
	}
}

// TestDockerReserveDefault: the Docker reserve is 3 GiB, or a quarter of the VM when that is
// smaller. Mutation: max instead of min gives 6501 and 3072.
func TestDockerReserveDefault(t *testing.T) {
	for v, want := range map[int64]int64{26005: 3072, 8000: 2000} {
		if got := DockerReserve(v); got != want {
			t.Errorf("DockerReserve(%d) = %d; want %d", v, got, want)
		}
	}
}
