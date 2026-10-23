package spec

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MemTokenMiB is the size of one token of the mem resource.
const MemTokenMiB = 128

var bytesRE = regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,3})?[GM]?$`)

// ParseBytes parses a memory size: a number of at most six digits and three decimals, then G
// (GiB, also meant when no suffix follows) or M (MiB). The result is in MiB, rounded up to a
// whole MiB.
func ParseBytes(s string) (int64, error) {
	if !bytesRE.MatchString(s) {
		return 0, fmt.Errorf("%q is not a size: want a number of GiB, such as 3 or 2.5G, or of MiB, such as 768M", s)
	}
	unit := int64(1024)
	switch s[len(s)-1] {
	case 'G':
		s = s[:len(s)-1]
	case 'M':
		unit, s = 1, s[:len(s)-1]
	}
	whole, frac, _ := strings.Cut(s, ".")
	frac += strings.Repeat("0", 3-len(frac))
	w, _ := strconv.ParseInt(whole, 10, 64)
	f, _ := strconv.ParseInt(frac, 10, 64)
	milli := (w*1000 + f) * unit
	return (milli + 999) / 1000, nil
}

// MemTokensCeil is the number of mem tokens a need of mib MiB asks for: rounded up, at most
// MaxPerResource.
func MemTokensCeil(mib int64) int {
	return int(min((mib+MemTokenMiB-1)/MemTokenMiB, MaxPerResource))
}

// MemTokensFloor is the number of whole mem tokens in mib MiB, never below 0.
func MemTokensFloor(mib int64) int {
	return int(max(mib/MemTokenMiB, 0))
}

// FormatMiB shows a size for a person: MiB below 1 GiB, GiB with one decimal above it.
func FormatMiB(mib int64) string {
	if mib < 1024 {
		return fmt.Sprintf("%d MiB", mib)
	}
	tenths := (mib*10 + 512) / 1024
	if tenths%10 == 0 {
		return fmt.Sprintf("%d GiB", tenths/10)
	}
	return fmt.Sprintf("%d.%d GiB", tenths/10, tenths%10)
}

// NeedText is a size as a need's value, which ParseBytes reads back exactly: whole GiB as "<n>G",
// anything else as "<n>M".
func NeedText(mib int64) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%dG", mib/1024)
	}
	return fmt.Sprintf("%dM", mib)
}

// parseMemNeed is ParseNeed for the mem resource: one size, never a range, more than 0.
func parseMemNeed(arg, value string) (Range, error) {
	if strings.Contains(value, "..") {
		return Range{}, usagef("invalid need %q: mem takes one size, such as mem=3G or mem=768M, not a range", arg)
	}
	mib, err := ParseBytes(value)
	if err != nil {
		return Range{}, usagef("invalid need %q: %v", arg, err)
	}
	if mib == 0 {
		return Range{}, usagef("invalid need %q: mem needs more than 0", arg)
	}
	t := MemTokensCeil(mib)
	return Range{Min: t, Max: t}, nil
}
