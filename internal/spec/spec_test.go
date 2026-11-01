package spec

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateBounds(t *testing.T) {
	base := func() Request {
		return Request{Label: "gate:unit", NeedMiB: 3072}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	edge := []struct {
		what  string
		mod   func(*Request)
		valid bool
	}{
		{"128-byte label", func(r *Request) { r.Label = strings.Repeat("é", 64) }, true},
		{"129-byte label", func(r *Request) { r.Label = strings.Repeat("é", 64) + "x" }, false},
		{"invalid UTF-8 label", func(r *Request) { r.Label = "gate\xff" }, false},
		{"need 0", func(r *Request) { r.NeedMiB = 0 }, false},
		{"need 1 MiB", func(r *Request) { r.NeedMiB = 1 }, true},
		{"need above the machine", func(r *Request) { r.NeedMiB = 40960 }, true},
		{"need MaxNeedMiB", func(r *Request) { r.NeedMiB = MaxNeedMiB }, true},
		{"need above MaxNeedMiB", func(r *Request) { r.NeedMiB = MaxNeedMiB + 1 }, false},
	}
	for _, tc := range edge {
		r := base()
		tc.mod(&r)
		err := r.Validate()
		if tc.valid && err != nil {
			t.Errorf("%s: unexpected error %v", tc.what, err)
		}
		if !tc.valid {
			var ue *UsageError
			if !errors.As(err, &ue) {
				t.Errorf("%s: got %v, want a *UsageError", tc.what, err)
			}
		}
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize(Request{Label: "x"}).NeedMiB; got != DefaultNeedMiB {
		t.Errorf("Normalize default need = %d MiB, want %d", got, DefaultNeedMiB)
	}
	if got := Normalize(Request{NeedMiB: 700}).NeedMiB; got != 700 {
		t.Errorf("Normalize changed an explicit need to %d MiB", got)
	}
}
