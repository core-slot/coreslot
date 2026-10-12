package wire

import (
	"errors"
	"testing"
)

func TestAdmitLockRoundTrip(t *testing.T) {
	body, err := EncodeAdmitLock(AdmitLock{PID: 4711, EnteredNS: 1791720000000000000})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"pid":4711,"entered_ns":1791720000000000000}`; string(body) != want {
		t.Fatalf("body %s, want %s", body, want)
	}
	a, err := DecodeAdmitLock(body)
	if err != nil || a != (AdmitLock{PID: 4711, EnteredNS: 1791720000000000000}) {
		t.Fatalf("decode: %+v, %v", a, err)
	}
}

// One rule for writer and readers: a positive pid and a non-negative entry time, in a body that
// follows section 7.2. Anything else is malformed, so no reader shows it.
func TestAdmitLockRule(t *testing.T) {
	for _, a := range []AdmitLock{{PID: 0, EnteredNS: 1}, {PID: -3, EnteredNS: 1}, {PID: 1, EnteredNS: -1}} {
		if _, err := EncodeAdmitLock(a); !errors.Is(err, ErrMalformed) {
			t.Errorf("encode %+v: %v, want ErrMalformed", a, err)
		}
	}
	for _, body := range []string{
		``,
		`{}`,
		`{"pid":0,"entered_ns":1}`,
		`{"pid":-3,"entered_ns":1}`,
		`{"pid":4711,"entered_ns":-1}`,
		`{"pid":4711}`,
		`{"pid":"4711","entered_ns":1}`,
		`{"pid":4711,"pid":4712,"entered_ns":1}`,
		`{"pid":4711,"entered_ns":1.5}`,
		`[4711,1]`,
	} {
		if a, err := DecodeAdmitLock([]byte(body)); !errors.Is(err, ErrMalformed) {
			t.Errorf("decode %q: %+v, %v; want ErrMalformed", body, a, err)
		}
	}
}
