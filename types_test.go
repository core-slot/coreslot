package coreslot

import (
	"errors"
	"testing"
)

// Validate takes a request as Enqueue and Adopt do: an empty Need is cpu 1..1, not an error.
func TestValidateNormalizes(t *testing.T) {
	for _, r := range []Request{{}, {Label: "empty need", Need: map[string]Range{}}} {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v.Validate() = %v, want nil: an empty Need is cpu 1..1", r, err)
		}
	}
	var ue *UsageError
	if err := (Request{Need: map[string]Range{"cpu": {Min: 3, Max: 2}}}).Validate(); !errors.As(err, &ue) {
		t.Errorf("min > max: %v, want a *UsageError", err)
	}
}

func TestErrUnsupportedMatchesTheStandardError(t *testing.T) {
	if !errors.Is(ErrUnsupported, errors.ErrUnsupported) {
		t.Errorf("ErrUnsupported (%v) does not match errors.ErrUnsupported", ErrUnsupported)
	}
}
