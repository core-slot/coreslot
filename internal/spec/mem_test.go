package spec

import "testing"

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
