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
		// Mutation: dropping the Docker bounds accepts the negative and the oversized values.
		{"Docker need MaxNeedMiB", func(r *Request) { r.DockerNeedMiB = MaxNeedMiB }, true},
		{"Docker need above MaxNeedMiB", func(r *Request) { r.DockerNeedMiB = MaxNeedMiB + 1 }, false},
		{"Docker need negative", func(r *Request) { r.DockerNeedMiB = -1 }, false},
		{"Docker footprint above MaxNeedMiB", func(r *Request) { r.DockerFootprintMiB = MaxNeedMiB + 1 }, false},
		{"Docker footprint negative", func(r *Request) { r.DockerFootprintMiB = -1 }, false},
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
