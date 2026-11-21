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

// TestGrowthZeroOnGrownVM: a VM at or past its size V takes nothing more from the host. Mutation:
// dropping the max(0, …) around V − vm_now gives −1000 for the VM past its size.
func TestGrowthZeroOnGrownVM(t *testing.T) {
	for _, vmNow := range []int64{8000, 9000} {
		if got := Growth(8000, vmNow, 4000, 0); got != 0 {
			t.Errorf("Growth(V 8000, vm_now %d, need 4000, footprint 0) = %d; want 0", vmNow, got)
		}
	}
}

// TestGrowthZeroWhenNeedFilled: containers that took their need cause no more growth. Mutation:
// dropping the max(0, …) around need − footprint gives −2000 for the need overfilled.
func TestGrowthZeroWhenNeedFilled(t *testing.T) {
	for _, fp := range []int64{4000, 5000} {
		if got := Growth(26005, 10000, 4000, fp); got != 0 {
			t.Errorf("Growth(V 26005, vm_now 10000, need 4000, footprint %d) = %d; want 0", fp, got)
		}
	}
}

// TestGrowthCappedByVM: what the VM can still take bounds the growth: V − vm_now is 1000 and the
// remaining need 4000, so 1000. Mutation: dropping the first term gives 8000.
func TestGrowthCappedByVM(t *testing.T) {
	if got := Growth(26005, 25005, 4000, 0); got != 1000 {
		t.Errorf("Growth = %d; want 1000", got)
	}
}

// TestGrowthTwiceRemaining: on a VM with room to grow, a hold reserves twice its remaining need:
// need 3000, footprint 1000, so 4000. Mutation: GrowthFactor = 1 gives 2000.
func TestGrowthTwiceRemaining(t *testing.T) {
	if got := Growth(26005, 6005, 3000, 1000); got != 4000 {
		t.Errorf("Growth = %d; want 4000", got)
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
