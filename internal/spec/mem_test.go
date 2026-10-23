package spec

import (
	"errors"
	"strings"
	"testing"
)

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{"3": 3072, "3G": 3072, "3.1G": 3175, "768M": 768,
		"0": 0, "1.5": 1536, "0.01G": 11, "2816M": 2816} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "3g", "3GB", "3GiB", "1.2345G", "1e3", " 3G", "3..4", ".5G", "1234567G"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A mem need is one size, rounded up to whole 128 MiB tokens and capped at MaxPerResource.
// Mutation: round down in MemTokensCeil, and mem=700M and mem=1M redden.
func TestParseNeedMem(t *testing.T) {
	for in, want := range map[string]Range{"mem=3G": {24, 24}, "mem=700M": {6, 6}, "mem=1M": {1, 1},
		"mem=40G": {256, 256}, "mem=2.5": {20, 20}} {
		name, r, err := ParseNeed(in)
		if err != nil || name != "mem" || r != want {
			t.Errorf("ParseNeed(%q) = %q, %+v, %v; want mem, %+v", in, name, r, err, want)
		}
	}
	for in, msg := range map[string]string{"mem=0": "more than 0", "mem=1..4": "not a range",
		"mem=..4G": "not a range", "mem=2G..": "not a range"} {
		_, _, err := ParseNeed(in)
		var ue *UsageError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), msg) {
			t.Errorf("ParseNeed(%q): got %v, want a *UsageError containing %q", in, err, msg)
		}
	}
}

func TestFormatMiB(t *testing.T) {
	for in, want := range map[int64]string{9728: "9.5 GiB", 8192: "8 GiB", 1100: "1.1 GiB",
		768: "768 MiB", 128: "128 MiB"} {
		if got := FormatMiB(in); got != want {
			t.Errorf("FormatMiB(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestNeedText(t *testing.T) {
	for in, want := range map[int64]string{3072: "3G", 2816: "2816M", 128: "128M"} {
		if got := NeedText(in); got != want {
			t.Errorf("NeedText(%d) = %q, want %q", in, got, want)
		}
	}
}
