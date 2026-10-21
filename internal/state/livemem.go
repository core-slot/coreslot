package state

import "github.com/core-slot/coreslot/internal/spec"

// MemHold is one live hold's memory reservation as the live size reads it.
type MemHold struct {
	Tokens  int   // mem tokens the hold lists
	UsedKiB int64 // resident memory of its tree; negative when not measured (the whole reservation counts)
}

// LiveMemSize returns mem's size for one admission attempt: max(static, min(4096, locked +
// ⌊room / 128 MiB⌋)), room = max(0, headroom − unrealised − unlisted), unrealised the sum of
// max(0, Tokens × 128 MiB − used) over holds, unlisted 128 MiB per locked token no hold lists.
// raised reports size > static.
func LiveMemSize(static, locked int, headroomMiB int64, holds []MemHold) (size int, raised bool) {
	var unrealised int64
	listed := 0
	for _, h := range holds {
		listed += h.Tokens
		reserved := int64(h.Tokens) * spec.MemTokenMiB
		if h.UsedKiB < 0 {
			unrealised += reserved
			continue
		}
		unrealised += max(0, reserved-h.UsedKiB/1024)
	}
	unlisted := int64(max(0, locked-listed)) * spec.MemTokenMiB
	room := max(0, headroomMiB-unrealised-unlisted)
	size = max(static, int(min(spec.MaxSize, int64(locked)+room/spec.MemTokenMiB)))
	return size, size > static
}
