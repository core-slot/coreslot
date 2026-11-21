package state

// MemHold is one live hold's reservation as admission reads it, in MiB.
type MemHold struct {
	NeedMiB      int64 // what the hold reserves
	FootprintMiB int64 // what its tree uses now; 0 when not known, so that the whole need counts
	GrowthMiB    int64 // the growth of Docker's VM the hold reserves on the host for its containers
}

// Room is what the live holds leave of headroomMiB: the headroom less the reserve, each hold's
// remaining growth, max(0, need − footprint), and the VM growth each hold reserves. It may be
// negative.
func Room(headroomMiB, reserveMiB int64, holds []MemHold) int64 {
	room := GrowthRoom(headroomMiB, reserveMiB, holds)
	for _, h := range holds {
		room -= max(0, h.NeedMiB-h.FootprintMiB)
	}
	return room
}

// GrowthRoom is a gated container request's room on the host: the headroom less the reserve and
// the VM growth every live hold reserves. It may be negative.
func GrowthRoom(headroomMiB, reserveMiB int64, holds []MemHold) int64 {
	room := headroomMiB - reserveMiB
	for _, h := range holds {
		room -= h.GrowthMiB
	}
	return room
}

// GrowthFactor is how much Docker's VM grew, at worst, per MiB its containers newly reached.
const GrowthFactor = 2

// Growth is the VM growth a gated hold reserves on the host: what the VM of size vMiB, holding
// vmNowMiB now, can still take, but no more than GrowthFactor times what the hold's containers
// have yet to reach of their Docker need. It is 0 on a VM grown to its size and once the
// containers have taken their need.
func Growth(vMiB, vmNowMiB, dockerNeedMiB, dockerFootprintMiB int64) int64 {
	return min(max(0, vMiB-vmNowMiB), GrowthFactor*max(0, dockerNeedMiB-dockerFootprintMiB))
}

// DockerHold is one live hold's Docker part as a gated run reads it, in MiB.
type DockerHold struct {
	NeedMiB      int64 // the hold's Docker need
	FootprintMiB int64 // what its containers use now
}

// DockerRoom is what is left inside Docker's VM: its estimated free memory less the Docker reserve
// and each hold's max(0, docker need − docker footprint). It may be negative.
func DockerRoom(freeEstMiB, dockerReserveMiB int64, holds []DockerHold) int64 {
	room := freeEstMiB - dockerReserveMiB
	for _, h := range holds {
		room -= max(0, h.NeedMiB-h.FootprintMiB)
	}
	return room
}

// DockerReserve is the part of a VM of size vMiB that no gated run counts on: 3 GiB, or a quarter
// of the VM when that is smaller.
func DockerReserve(vMiB int64) int64 {
	return min(3072, vMiB/4)
}
