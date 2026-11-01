package spec

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateBounds(t *testing.T) {
	base := func() Request {
		return Request{Label: "gate:unit", Priority: 0, Need: map[string]Range{"mem": {Min: 4, Max: 12}}}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}

	nine := map[string]Range{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		nine[n] = Range{Min: 1, Max: 1}
	}
	eight := map[string]Range{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		eight[n] = Range{Min: 1, Max: 1}
	}

	edge := []struct {
		what  string
		mod   func(*Request)
		valid bool
	}{
		{"8 resources", func(r *Request) { r.Need = eight }, true},
		{"9 resources", func(r *Request) { r.Need = nine }, false},
		{"no resources", func(r *Request) { r.Need = map[string]Range{} }, false},
		{"priority 1000", func(r *Request) { r.Priority = 1000 }, true},
		{"priority 1001", func(r *Request) { r.Priority = 1001 }, false},
		{"priority -1000", func(r *Request) { r.Priority = -1000 }, true},
		{"priority -1001", func(r *Request) { r.Priority = -1001 }, false},
		{"128-byte label", func(r *Request) { r.Label = strings.Repeat("é", 64) }, true},
		{"129-byte label", func(r *Request) { r.Label = strings.Repeat("é", 64) + "x" }, false},
		{"invalid UTF-8 label", func(r *Request) { r.Label = "gate\xff" }, false},
		{"invalid resource name", func(r *Request) { r.Need = map[string]Range{"Mem": {Min: 1, Max: 1}} }, false},
		{"min 0", func(r *Request) { r.Need = map[string]Range{"mem": {Min: 0, Max: 1}} }, false},
		{"min above max", func(r *Request) { r.Need = map[string]Range{"mem": {Min: 3, Max: 2}} }, false},
		{"max 257", func(r *Request) { r.Need = map[string]Range{"mem": {Min: 1, Max: 257}} }, false},
		{"minimums summing to 512", func(r *Request) {
			r.Need = map[string]Range{"mem": {Min: 256, Max: 256}, "gpu": {Min: 256, Max: 256}}
		}, true},
		{"minimums summing to 600", func(r *Request) {
			r.Need = map[string]Range{"a": {Min: 200, Max: 256}, "b": {Min: 200, Max: 256}, "c": {Min: 200, Max: 256}}
		}, false},
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

func TestEnvName(t *testing.T) {
	if got := EnvName("pg-local"); got != "PG_LOCAL" {
		t.Errorf(`EnvName("pg-local") = %q, want "PG_LOCAL"`, got)
	}
	if got := EnvName("mem"); got != "MEM" {
		t.Errorf(`EnvName("mem") = %q, want "MEM"`, got)
	}
	for _, bad := range []string{"pg_local", "Pg", "", "1mem", "-mem", strings.Repeat("a", 33), "mem.x"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) accepted an invalid name", bad)
		}
	}
	for _, good := range []string{"mem", "pg-local", "a", strings.Repeat("a", 32), "gpu-0"} {
		if err := ValidateName(good); err != nil {
			t.Errorf("ValidateName(%q): unexpected error %v", good, err)
		}
	}
}

func TestNormalize(t *testing.T) {
	in := Request{Label: "x"}
	out := Normalize(in)
	if len(out.Need) != 1 || out.Need["mem"] != (Range{Min: 1, Max: 1}) {
		t.Errorf("Normalize default need = %v, want mem=1..1", out.Need)
	}
	if in.Need != nil {
		t.Error("Normalize modified its argument")
	}
	given := Request{Need: map[string]Range{"pg-local": {Min: 1, Max: 1}}}
	out = Normalize(given)
	if _, ok := out.Need["mem"]; ok || len(out.Need) != 1 {
		t.Errorf("Normalize added mem to an explicit need: %v", out.Need)
	}
	out.Need["extra"] = Range{Min: 1, Max: 1}
	if _, ok := given.Need["extra"]; ok {
		t.Error("Normalize returned the caller's map")
	}
}
