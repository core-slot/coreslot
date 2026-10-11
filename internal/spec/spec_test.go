package spec

import (
	"errors"
	"strings"
	"testing"
)

func TestParseNeed(t *testing.T) {
	valid := []struct {
		arg  string
		name string
		want Range
	}{
		{"cpu=4..12", "cpu", Range{Min: 4, Max: 12}},
		{"cpu=8", "cpu", Range{Min: 8, Max: 8}},
		{"pg-local=1", "pg-local", Range{Min: 1, Max: 1}},
		{"cpu=1..256", "cpu", Range{Min: 1, Max: 256}},
	}
	for _, tc := range valid {
		name, r, err := ParseNeed(tc.arg)
		if err != nil {
			t.Errorf("ParseNeed(%q): unexpected error %v", tc.arg, err)
			continue
		}
		if name != tc.name || r != tc.want {
			t.Errorf("ParseNeed(%q) = %q, %+v; want %q, %+v", tc.arg, name, r, tc.name, tc.want)
		}
	}

	invalid := []string{
		"cpu=0", "cpu=5..4", "cpu=-1", "cpu=257", "CPU=1", "cpu=1..x", "=1",
		"cpu", "cpu=", "cpu=..4", "cpu=1..", "cpu=+1", "cpu=1..2..3",
	}
	for _, arg := range invalid {
		_, _, err := ParseNeed(arg)
		var ue *UsageError
		if !errors.As(err, &ue) {
			t.Errorf("ParseNeed(%q): got %v, want a *UsageError", arg, err)
		}
	}
}

func TestValidateBounds(t *testing.T) {
	base := func() Request {
		return Request{Label: "gate:unit", Priority: 0, Need: map[string]Range{"cpu": {Min: 4, Max: 12}}}
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
		{"invalid resource name", func(r *Request) { r.Need = map[string]Range{"Cpu": {Min: 1, Max: 1}} }, false},
		{"min 0", func(r *Request) { r.Need = map[string]Range{"cpu": {Min: 0, Max: 1}} }, false},
		{"min above max", func(r *Request) { r.Need = map[string]Range{"cpu": {Min: 3, Max: 2}} }, false},
		{"max 257", func(r *Request) { r.Need = map[string]Range{"cpu": {Min: 1, Max: 257}} }, false},
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

	// A duplicate resource can only arise while --need arguments are collected, because a
	// Request's Need is a map.
	if _, err := ParseNeeds([]string{"cpu=2", "pg-local=1", "cpu=4..8"}); err == nil {
		t.Error("ParseNeeds accepted a duplicate resource")
	} else {
		var ue *UsageError
		if !errors.As(err, &ue) {
			t.Errorf("duplicate resource: got %v, want a *UsageError", err)
		}
	}
	need, err := ParseNeeds([]string{"cpu=2..6", "pg-local=1"})
	if err != nil {
		t.Fatalf("ParseNeeds: %v", err)
	}
	if len(need) != 2 || need["cpu"] != (Range{Min: 2, Max: 6}) || need["pg-local"] != (Range{Min: 1, Max: 1}) {
		t.Errorf("ParseNeeds = %v", need)
	}
}

func TestEnvName(t *testing.T) {
	if got := EnvName("pg-local"); got != "PG_LOCAL" {
		t.Errorf(`EnvName("pg-local") = %q, want "PG_LOCAL"`, got)
	}
	if got := EnvName("cpu"); got != "CPU" {
		t.Errorf(`EnvName("cpu") = %q, want "CPU"`, got)
	}
	for _, bad := range []string{"pg_local", "Pg", "", "1cpu", "-cpu", strings.Repeat("a", 33), "cpu.x"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) accepted an invalid name", bad)
		}
	}
	for _, good := range []string{"cpu", "pg-local", "a", strings.Repeat("a", 32), "gpu-0"} {
		if err := ValidateName(good); err != nil {
			t.Errorf("ValidateName(%q): unexpected error %v", good, err)
		}
	}
}

func TestNormalize(t *testing.T) {
	in := Request{Label: "x"}
	out := Normalize(in)
	if len(out.Need) != 1 || out.Need["cpu"] != (Range{Min: 1, Max: 1}) {
		t.Errorf("Normalize default need = %v, want cpu=1..1", out.Need)
	}
	if in.Need != nil {
		t.Error("Normalize modified its argument")
	}
	given := Request{Need: map[string]Range{"pg-local": {Min: 1, Max: 1}}}
	out = Normalize(given)
	if _, ok := out.Need["cpu"]; ok || len(out.Need) != 1 {
		t.Errorf("Normalize added cpu to an explicit need: %v", out.Need)
	}
	out.Need["extra"] = Range{Min: 1, Max: 1}
	if _, ok := given.Need["extra"]; ok {
		t.Error("Normalize returned the caller's map")
	}
}
