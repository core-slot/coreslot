package state

// MemHold is one live hold's reservation as admission reads it, in MiB.
type MemHold struct {
	NeedMiB      int64 // what the hold reserves
	FootprintMiB int64 // what its tree uses now; 0 when not known, so that the whole need counts
}

// Room is what the live holds leave of headroomMiB: the headroom less the reserve and each
// hold's remaining growth, max(0, need − footprint). It may be negative.
func Room(headroomMiB, reserveMiB int64, holds []MemHold) int64 {
	room := headroomMiB - reserveMiB
	for _, h := range holds {
		room -= max(0, h.NeedMiB-h.FootprintMiB)
	}
	return room
}
