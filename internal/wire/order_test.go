package wire

import (
	"errors"
	"testing"
)

// An order override is {"order_ns": n} with n at least 0, under the rules of every body; it round
// trips through Encode. Mutations: accepting a negative order_ns, or a missing key, reddens a row.
func TestOrderBody(t *testing.T) {
	b, err := Encode(Order{OrderNS: 42})
	if err != nil || string(b) != `{"order_ns":42}` {
		t.Fatalf("Encode = %s, %v", b, err)
	}
	if o, err := DecodeOrder(b); err != nil || o.OrderNS != 42 {
		t.Fatalf("DecodeOrder(%s) = %+v, %v", b, o, err)
	}
	if _, err := Encode(Order{OrderNS: -1}); !errors.Is(err, ErrMalformed) {
		t.Errorf("Encode of a negative order_ns: %v, want malformed", err)
	}
	for _, body := range []string{`{"order_ns":-1}`, `{}`, `{"order_ns":1,"order_ns":2}`, `{"order_ns":1.5}`,
		`{"order_ns":"1"}`, `{"order_ns":9223372036854775808}`, `[1]`} {
		if _, err := DecodeOrder([]byte(body)); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeOrder(%s) = %v, want malformed", body, err)
		}
	}
	if o, err := DecodeOrder([]byte(` {"order_ns":9223372036854775807,"later":[1]} `)); err != nil || o.OrderNS != 1<<63-1 {
		t.Errorf("the largest key with an unknown key: %+v, %v", o, err)
	}
}
